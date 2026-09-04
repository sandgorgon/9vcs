package main

import (
	"fmt"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

func cmdDiff(args []string) error {
	if len(args) > 2 {
		return fmt.Errorf("diff: too many arguments (expected [<ref>] or [<ref> <ref>])")
	}

	r, err := repo.Find()
	if err != nil {
		return err
	}

	if len(args) == 2 {
		return diffRefs(r, args[0], args[1])
	}

	var base patches.Hash
	if len(args) == 1 {
		base, err = r.ResolveRef(args[0])
		if err != nil {
			return fmt.Errorf("diff: %w", err)
		}
	} else {
		base, _, err = r.HeadHash()
		if err != nil {
			return fmt.Errorf("reading head: %w", err)
		}
	}
	return diffWorkingTree(r, base)
}

// diffWorkingTree prints the difference between the materialized state at
// base and the actual working-tree files — the uncommitted-changes view.
func diffWorkingTree(r *repo.Repo, base patches.Hash) error {
	idx, err := r.Materialize(base)
	if err != nil {
		return fmt.Errorf("replaying history: %w", err)
	}
	changes, err := repo.ChangedFiles(r, idx)
	if err != nil {
		return err
	}
	renderChanges(idx, changes)
	return nil
}

// renderChanges is diffWorkingTree and diffRefs' shared tail.
func renderChanges(base patches.Index, changes map[string]patches.FileChange) {
	for _, p := range repo.SortedPaths(changes) {
		renderDiff(p, base[p], changes[p])
	}
}

// diffRefs prints the difference between two materialized points in
// history directly, without involving the working tree at all.
func diffRefs(r *repo.Repo, a, b string) error {
	ha, err := r.ResolveRef(a)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}
	hb, err := r.ResolveRef(b)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}
	idxA, err := r.Materialize(ha)
	if err != nil {
		return fmt.Errorf("replaying %s: %w", a, err)
	}
	idxB, err := r.Materialize(hb)
	if err != nil {
		return fmt.Errorf("replaying %s: %w", b, err)
	}

	paths := map[string]bool{}
	for p := range idxA {
		paths[p] = true
	}
	for p := range idxB {
		paths[p] = true
	}
	changes := map[string]patches.FileChange{}
	for p := range paths {
		stA, inA := idxA[p]
		stB, inB := idxB[p]

		switch {
		case !inB:
			changes[p] = patches.FileChange{Path: p, Kind: patches.KindDelete}
		case stB.Kind == patches.KindBlob:
			if inA && stA.Kind == patches.KindBlob && stA.Blob == stB.Blob && stA.Executable == stB.Executable {
				continue // unchanged
			}
			changes[p] = patches.FileChange{Path: p, Kind: patches.KindBlob, Blob: stB.Blob, Executable: stB.Executable}
		case stB.Kind == patches.KindSymlink:
			if inA && stA.Kind == patches.KindSymlink && stA.SymlinkTarget == stB.SymlinkTarget {
				continue // unchanged
			}
			changes[p] = patches.FileChange{Path: p, Kind: patches.KindSymlink, SymlinkTarget: stB.SymlinkTarget}
		default: // stB is text
			baseLinesA := linesOf(stA)
			newLinesB := linesOf(stB)
			newContent := make([]string, len(newLinesB))
			for i, l := range newLinesB {
				newContent[i] = l.Content
			}
			ops, _ := patches.Diff(baseLinesA, newContent)
			if len(ops) == 0 && inA && stA.Kind == patches.KindText && stA.TrailingNewline == stB.TrailingNewline && stA.Executable == stB.Executable {
				continue // unchanged
			}
			changes[p] = patches.FileChange{Path: p, Kind: patches.KindText, Ops: ops, TrailingNewline: stB.TrailingNewline, Executable: stB.Executable}
		}
	}
	renderChanges(idxA, changes)
	return nil
}

// linesOf renders a text PathState's current content, ignoring any
// unresolved forks (diff is read-only display; a real fork only ever
// shows up here for an in-progress merge's own patch, which diff doesn't
// need to render specially — record's conflict-marker working-tree file is
// what the user actually edits).
func linesOf(st patches.PathState) []patches.Line {
	if st.Kind != patches.KindText || st.Graph == nil {
		return nil
	}
	lines, _ := patches.Linearize(st.Graph)
	return lines
}

// renderDiff prints a diff for one path's change. Text changes get a
// simple +/- line diff (not grouped into unified-diff hunks with
// surrounding context — a scaffold-quality rendering, not a full diff
// formatter). Binary changes get git's own "Binary files ... differ"
// treatment, since a line diff is meaningless for them.
func renderDiff(path string, base patches.PathState, change patches.FileChange) {
	baseLines := linesOf(base)
	switch change.Kind {
	case patches.KindDelete:
		switch base.Kind {
		case patches.KindBlob:
			fmt.Printf("deleted binary file %s\n", path)
		case patches.KindSymlink:
			fmt.Printf("deleted symlink %s -> %s\n", path, base.SymlinkTarget)
		default:
			if len(baseLines) == 0 {
				return
			}
			fmt.Printf("--- %s\n+++ /dev/null\n", path)
			for _, l := range baseLines {
				fmt.Printf("-%s\n", l.Content)
			}
		}
	case patches.KindSymlink:
		if base.Kind == patches.KindSymlink {
			fmt.Printf("symlink %s: %s -> %s\n", path, base.SymlinkTarget, change.SymlinkTarget)
		} else {
			fmt.Printf("new symlink %s -> %s\n", path, change.SymlinkTarget)
		}
	case patches.KindBlob:
		if base.Kind == patches.KindBlob && base.Blob == change.Blob {
			printModeChange(path, base.Executable, change.Executable)
			return
		}
		fmt.Printf("Binary files %s and %s differ\n", path, path)
	default: // KindText
		// OpSever/OpLink (fork-healing ops from patches.Resolve, or now
		// also from Diff collapsing a same-patch multi-line delete run —
		// see diff.go's emitGap) carry no visible content of their own;
		// only Insert/Delete do. Treating a changeset that's entirely
		// Sever/Link the same as no ops at all avoids printing an empty
		// "--- / +++" header with nothing under it, which reads as "diff
		// found no difference" even though the path is genuinely dirty.
		hasContentOp := false
		for _, op := range change.Ops {
			if op.Kind == patches.OpInsert || op.Kind == patches.OpDelete {
				hasContentOp = true
				break
			}
		}
		if !hasContentOp {
			if base.Kind == patches.KindText {
				printModeChange(path, base.Executable, change.Executable)
			}
			return
		}
		baseContent := make(map[string]string, len(baseLines))
		for _, l := range baseLines {
			baseContent[l.ID] = l.Content
		}
		fmt.Printf("--- %s\n+++ %s\n", path, path)
		for _, op := range change.Ops {
			switch op.Kind {
			case patches.OpDelete:
				fmt.Printf("-%s\n", baseContent[op.ID])
			case patches.OpInsert:
				fmt.Printf("+%s\n", op.Content)
			}
		}
	}
}

// printModeChange prints a minimal note for the case changedFiles/diffRefs'
// unchanged-checks now catch: content is byte-for-byte (or line-for-line)
// identical and only the executable bit flipped. Without this, that change
// would still be correctly recorded (changedFiles doesn't miss it) but
// would render as if diff found nothing at all — misleading, even though
// nothing is actually wrong.
func printModeChange(path string, wasExecutable, isExecutable bool) {
	if wasExecutable == isExecutable {
		return
	}
	if isExecutable {
		fmt.Printf("mode changed: %s (now executable)\n", path)
	} else {
		fmt.Printf("mode changed: %s (no longer executable)\n", path)
	}
}
