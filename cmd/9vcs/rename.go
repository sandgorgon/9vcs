package main

import (
	"sort"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// renameThreshold is the minimum similarity a KindDelete/KindText-add pair
// must clear to be reported as a rename (with modifications) rather than
// two unrelated changes — git's own default for -M/--find-renames is 50%,
// reused here for the same reason: low enough to still catch a rename
// alongside a real edit, high enough that two small, unrelated files
// don't pair up by coincidence.
const renameThreshold = 0.5

// renamePair is one detected rename — purely a display-time inference,
// never anything record actually stores. See PLAN.md's "Rename detection
// — concrete scope": the underlying patch is still a plain delete of
// oldPath plus a fresh insert under newPath, byte-for-byte identical to
// what was recorded before this feature existed. diffOps, when newKind is
// KindText, is a real diff between the old and new content (not the
// insert-only ops changedFiles/diffRefs originally computed against an
// empty base) — computed once, during scoring, and carried here so a
// caller rendering a modified rename doesn't redo the work.
type renamePair struct {
	oldPath, newPath string
	oldState         patches.PathState
	newKind          patches.ChangeKind
	newBlob          patches.Hash     // set when newKind == KindBlob
	diffOps          []patches.LineOp // set when newKind == KindText
	modified         bool
}

// detectRenames pairs changes' KindDelete entries with its new (not
// present in base) KindText/KindBlob entries by content similarity,
// greedily assigning each deleted path its best match above
// renameThreshold — each path consumed by at most one pairing. Returns
// the detected pairs (sorted by oldPath, for deterministic output) and a
// copy of changes with every consumed path removed, so a caller renders
// whatever's left with its normal added/modified/deleted logic.
func detectRenames(changes map[string]patches.FileChange, base patches.Index) ([]renamePair, map[string]patches.FileChange) {
	var deletedPaths, addedPaths []string
	for p, fc := range changes {
		if fc.Kind == patches.KindDelete {
			deletedPaths = append(deletedPaths, p)
			continue
		}
		if _, existed := base[p]; !existed {
			addedPaths = append(addedPaths, p)
		}
	}
	if len(deletedPaths) == 0 || len(addedPaths) == 0 {
		return nil, changes
	}

	type scored struct {
		score float64
		pair  renamePair
	}
	var candidates []scored
	for _, op := range deletedPaths {
		oldSt := base[op]
		for _, np := range addedPaths {
			if score, pair, ok := renameCandidate(op, oldSt, np, changes[np]); ok {
				candidates = append(candidates, scored{score, pair})
			}
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })

	usedOld := map[string]bool{}
	usedNew := map[string]bool{}
	var renames []renamePair
	for _, c := range candidates {
		if usedOld[c.pair.oldPath] || usedNew[c.pair.newPath] {
			continue
		}
		usedOld[c.pair.oldPath] = true
		usedNew[c.pair.newPath] = true
		renames = append(renames, c.pair)
	}
	if len(renames) == 0 {
		return nil, changes
	}
	sort.Slice(renames, func(i, j int) bool { return renames[i].oldPath < renames[j].oldPath })

	remaining := make(map[string]patches.FileChange, len(changes))
	for p, fc := range changes {
		if usedOld[p] || usedNew[p] {
			continue
		}
		remaining[p] = fc
	}
	return renames, remaining
}

// renameCandidate scores one (deleted, added) pair. A binary pair only
// ever matches exactly (no partial similarity metric for opaque content);
// a text pair's score is a symmetric overlap ratio — 2*unchanged /
// (len(old)+len(new)) — computed from the same patches.Diff used
// everywhere else in this codebase for line-level comparison, not a
// separate similarity algorithm.
func renameCandidate(oldPath string, oldSt patches.PathState, newPath string, newFc patches.FileChange) (score float64, pair renamePair, ok bool) {
	if oldSt.Kind != newFc.Kind {
		return 0, renamePair{}, false // a text file "renamed" to binary (or vice versa) isn't a rename
	}
	switch newFc.Kind {
	case patches.KindBlob:
		if oldSt.Blob != newFc.Blob {
			return 0, renamePair{}, false
		}
		return 1.0, renamePair{oldPath: oldPath, newPath: newPath, oldState: oldSt, newKind: patches.KindBlob, newBlob: newFc.Blob, modified: false}, true
	case patches.KindSymlink:
		// Same exact-match-only policy as KindBlob: a symlink retargeted
		// during the same change it was renamed in isn't detected as a
		// rename — there's no meaningful partial-similarity metric for a
		// target string the way there is for line content.
		if oldSt.SymlinkTarget != newFc.SymlinkTarget {
			return 0, renamePair{}, false
		}
		return 1.0, renamePair{oldPath: oldPath, newPath: newPath, oldState: oldSt, newKind: patches.KindSymlink, modified: false}, true
	default: // KindText
		oldLines := linesOf(oldSt)
		newContent := insertedContent(newFc.Ops)
		if len(oldLines) == 0 || len(newContent) == 0 {
			return 0, renamePair{}, false // no rename detection for an empty file on either side — nothing to meaningfully compare
		}
		ops, _ := patches.Diff(oldLines, newContent)
		deleted := 0
		for _, op := range ops {
			if op.Kind == patches.OpDelete {
				deleted++
			}
		}
		unchanged := len(oldLines) - deleted
		similarity := 2 * float64(unchanged) / float64(len(oldLines)+len(newContent))
		if similarity < renameThreshold {
			return 0, renamePair{}, false
		}
		return similarity, renamePair{oldPath: oldPath, newPath: newPath, oldState: oldSt, newKind: patches.KindText, diffOps: ops, modified: len(ops) != 0}, true
	}
}

// insertedContent reads out an insert-only ops list's content in order —
// exactly what changedFiles/diffRefs produce for a path with no prior
// state (patches.Diff against a nil/empty base yields all-Insert ops), so
// this reconstructs that path's full new content without needing to
// touch the working tree or object store again.
func insertedContent(ops []patches.LineOp) []string {
	var out []string
	for _, op := range ops {
		if op.Kind == patches.OpInsert {
			out = append(out, op.Content)
		}
	}
	return out
}
