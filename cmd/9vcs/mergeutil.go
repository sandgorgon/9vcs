package main

import (
	"fmt"
	"sort"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// mergeConflict is one path computeMerge couldn't resolve automatically.
type mergeConflict struct {
	Path string
	Kind string // "text", "binary", "modify/delete", "symlink", "type"
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

	// Binary, symlink, and cross-kind ("type") conflicts: more than one
	// distinct value — or more than one fundamentally different kind of
	// file — among roots for the same path, none of which a line-level
	// merge can resolve. Checked across every root that has the path,
	// not just roots[0] ("ours") — a path only roots[1] and roots[2]
	// both introduce, with different content, is exactly as much a
	// conflict as one "ours" also touched; anchoring solely on idxs[0]
	// silently missed it (Materialize's union just picked whichever
	// patch applied later in topological order, with no error). The
	// anchor is whichever root comes first in roots order that has the
	// path at all (any kind, not just Blob/Symlink) — preserves the
	// original two-way "ours wins" policy as the case where roots[0]
	// does have it, and falls through to the next root when it doesn't.
	//
	// A kind mismatch (e.g. text on one root, a blob on another) is
	// always its own "type" conflict, checked before any same-kind
	// value comparison: there's no shared line-graph to fork and no
	// atomic value to compare across kinds, so it can't be folded into
	// "binary" or "symlink" — and mustn't be, since merge.go/apply.go's
	// "binary" sidecar-writing code assumes the conflicting side really
	// is a KindBlob PathState with a real Blob hash to fetch; reporting
	// a text-vs-blob mismatch as "binary" would feed it a zero-value
	// Hash instead and fail with a confusing store-lookup error instead
	// of a clean conflict report.
	allPaths := map[string]bool{}
	for _, idx := range idxs {
		for p := range idx {
			allPaths[p] = true
		}
	}
	for p := range allPaths {
		var present []int
		for i, idx := range idxs {
			if _, ok := idx[p]; ok {
				present = append(present, i)
			}
		}
		if len(present) < 2 {
			continue // only one root (or none) has this path at all — nothing to disagree with
		}
		anchor := idxs[present[0]][p]

		kindMismatch := false
		valueConflict := false
		seenTargets := map[string]bool{}
		var symlinkOthers []string
		for _, i := range present[1:] {
			st := idxs[i][p]
			if st.Kind != anchor.Kind {
				kindMismatch = true
				continue
			}
			switch anchor.Kind {
			case patches.KindBlob:
				if st.Blob != anchor.Blob {
					valueConflict = true
				}
			case patches.KindSymlink:
				if st.SymlinkTarget != anchor.SymlinkTarget && !seenTargets[st.SymlinkTarget] {
					valueConflict = true
					seenTargets[st.SymlinkTarget] = true
					symlinkOthers = append(symlinkOthers, st.SymlinkTarget)
				}
			}
		}

		switch {
		case kindMismatch:
			add(mergeConflict{Path: p, Kind: "type"})
			merged[p] = anchor
		case valueConflict && anchor.Kind == patches.KindBlob:
			add(mergeConflict{Path: p, Kind: "binary"})
			merged[p] = anchor
		case valueConflict && anchor.Kind == patches.KindSymlink:
			sort.Strings(symlinkOthers)
			add(mergeConflict{Path: p, Kind: "symlink", OtherTargets: symlinkOthers})
			merged[p] = anchor
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
