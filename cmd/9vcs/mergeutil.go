package main

import (
	"fmt"
	"sort"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// mergeConflict is one path computeMerge couldn't resolve automatically.
type mergeConflict struct {
	Path string
	Kind string // "text", "binary", "modify/delete", "symlink"
	// DeletedBy identifies the deleting side for a "modify/delete"
	// conflict: "ours"/"theirs" for a plain two-root merge (kept as-is,
	// rather than renamed, so existing two-way conflict messages don't
	// change); a short hash once there are more than two roots and
	// "ours"/"theirs" stops meaning anything — see sideLabel.
	DeletedBy string
	// OtherTargets is "symlink" only: every distinct target among the
	// other roots (ours' own target is kept and isn't repeated here). A
	// symlink target is a short, human-readable string, unlike blob
	// content, so the conflict message can just list them inline —
	// no comparison sidecar file needed the way a binary conflict gets.
	OtherTargets []string
}

// sideLabel names roots[i] for a conflict message.
func sideLabel(roots []patches.Hash, i int) string {
	if len(roots) == 2 {
		if i == 0 {
			return "ours"
		}
		return "theirs"
	}
	return roots[i].String()[:12]
}

// computeMerge unions roots (two for a plain merge, more for an N-way
// apply), resolving what Materialize's plain union can't on its own:
//
//   - a binary path more than one root changed to a different blob — no
//     line-graph fork mechanism applies to whole-file content, so this is
//     checked directly, keeping roots[0]'s ("ours") content in the
//     result;
//   - a modify/delete race, one root deleting a path another root
//     genuinely changed — a delete and an edit to the same path don't
//     fork the way two competing inserts do, so Materialize's union
//     would otherwise just silently pick whichever patch applies later
//     in the deterministic topological order. Detected via
//     UniqueChanges, resolved by keeping the modifying side's content.
//
// Text-conflict detection needs no N-way-specific code at all: a fork is
// just "a node with more than one alive outgoing edge" regardless of how
// many roots contributed to it, and Linearize already reports however
// many alternatives exist — see linearize.go's walkFrom/Resolve.
//
// merge and record both need the exact same resolution — merge to decide
// what to write to the working tree, record to compute a base that
// actually matches what's on disk mid-merge — so this is the one place it
// happens, called by both (and, for N > 2, by apply).
func computeMerge(r *repo, roots ...patches.Hash) (patches.Index, []mergeConflict, error) {
	merged, err := r.materialize(roots...)
	if err != nil {
		return nil, nil, fmt.Errorf("replaying merged history: %w", err)
	}
	idxs := make([]patches.Index, len(roots))
	for i, root := range roots {
		idxs[i], err = r.materialize(root)
		if err != nil {
			return nil, nil, fmt.Errorf("replaying history for %s: %w", root, err)
		}
	}

	byPath := map[string]mergeConflict{}
	add := func(c mergeConflict) { byPath[c.Path] = c }

	// Binary and symlink conflicts: more than one distinct value among
	// roots for the same atomic (non-line-graph) path — a whole-file
	// blob or a symlink target, neither of which has a line-level merge
	// that makes sense. Checked across every root that has the path, not
	// just roots[0] ("ours") — a path only roots[1] and roots[2] both
	// introduce, with different content, is exactly as much a conflict
	// as one "ours" also touched; anchoring solely on idxs[0] silently
	// missed it (Materialize's union just picked whichever patch applied
	// later in topological order, with no error). The kept content is
	// whichever root comes first in roots order that has the path at
	// all — preserves the original two-way "ours wins" policy as the
	// case where roots[0] does have it, and falls through to the next
	// root when it doesn't.
	allPaths := map[string]bool{}
	for _, idx := range idxs {
		for p := range idx {
			allPaths[p] = true
		}
	}
	for p := range allPaths {
		anchor := -1
		for i, idx := range idxs {
			if st, ok := idx[p]; ok && (st.Kind == patches.KindBlob || st.Kind == patches.KindSymlink) {
				anchor = i
				break
			}
		}
		if anchor == -1 {
			continue
		}
		anchorSt := idxs[anchor][p]
		switch anchorSt.Kind {
		case patches.KindBlob:
			conflicted := false
			for i := anchor + 1; i < len(idxs); i++ {
				if st, ok := idxs[i][p]; ok && st.Kind == patches.KindBlob && st.Blob != anchorSt.Blob {
					conflicted = true
				}
			}
			if conflicted {
				add(mergeConflict{Path: p, Kind: "binary"})
				merged[p] = anchorSt
			}
		case patches.KindSymlink:
			seen := map[string]bool{}
			var others []string
			for i := anchor + 1; i < len(idxs); i++ {
				if st, ok := idxs[i][p]; ok && st.Kind == patches.KindSymlink && st.SymlinkTarget != anchorSt.SymlinkTarget && !seen[st.SymlinkTarget] {
					seen[st.SymlinkTarget] = true
					others = append(others, st.SymlinkTarget)
				}
			}
			if len(others) > 0 {
				sort.Strings(others)
				add(mergeConflict{Path: p, Kind: "symlink", OtherTargets: others})
				merged[p] = anchorSt
			}
		}
	}

	for p, st := range merged {
		if st.Kind != patches.KindText || st.Graph == nil {
			continue
		}
		if _, forks := patches.Linearize(st.Graph); len(forks) > 0 {
			add(mergeConflict{Path: p, Kind: "text"})
		}
	}

	// Modify/delete races: for each root, what it uniquely deleted and
	// modified relative to the union of every *other* root's closure —
	// computed once per root (UniqueChanges already reports both
	// together), not once per pair. A path deleted by root i and
	// modified by a different root j is a race, resolved by keeping j's
	// content.
	deletedBy := make([]map[string]bool, len(roots))
	modifiedBy := make([]map[string]bool, len(roots))
	for i, root := range roots {
		var others []patches.Hash
		for j, o := range roots {
			if j != i {
				others = append(others, o)
			}
		}
		othersClosure, err := patches.Closure(r.store, others...)
		if err != nil {
			return nil, nil, fmt.Errorf("replaying history for %s: %w", root, err)
		}
		deletedBy[i], modifiedBy[i], err = patches.UniqueChanges(r.store, root, othersClosure)
		if err != nil {
			return nil, nil, err
		}
	}
	for i := range roots {
		for j := range roots {
			if i == j {
				continue
			}
			for p := range deletedBy[i] {
				if !modifiedBy[j][p] {
					continue
				}
				st, ok := idxs[j][p]
				if !ok {
					continue
				}
				add(mergeConflict{Path: p, Kind: "modify/delete", DeletedBy: sideLabel(roots, i)})
				merged[p] = st
			}
		}
	}

	conflicts := make([]mergeConflict, 0, len(byPath))
	for _, c := range byPath {
		conflicts = append(conflicts, c)
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })

	return merged, conflicts, nil
}
