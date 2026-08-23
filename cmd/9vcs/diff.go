package main

import (
	"fmt"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func cmdDiff(args []string) error {
	if len(args) > 2 {
		return fmt.Errorf("diff: too many arguments (expected [<ref>] or [<ref> <ref>])")
	}

	r, err := findRepo()
	if err != nil {
		return err
	}

	if len(args) == 2 {
		return diffRefs(r, args[0], args[1])
	}

	var base patches.Hash
	if len(args) == 1 {
		base, err = r.resolveRef(args[0])
		if err != nil {
			return fmt.Errorf("diff: %w", err)
		}
	} else {
		base, _, err = r.headHash()
		if err != nil {
			return fmt.Errorf("reading head: %w", err)
		}
	}
	return diffWorkingTree(r, base)
}

// diffWorkingTree prints the difference between the materialized state at
// base and the actual working-tree files — the uncommitted-changes view.
func diffWorkingTree(r *repo, base patches.Hash) error {
	idx, err := patches.Materialize(r.store, base)
	if err != nil {
		return fmt.Errorf("replaying history: %w", err)
	}
	ops, err := changedFiles(r, idx)
	if err != nil {
		return err
	}
	for _, p := range sortedKeys(ops) {
		renderDiff(p, idx[p], ops[p])
	}
	return nil
}

// diffRefs prints the difference between two materialized points in
// history directly, without involving the working tree at all.
func diffRefs(r *repo, a, b string) error {
	ha, err := r.resolveRef(a)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}
	hb, err := r.resolveRef(b)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}
	idxA, err := patches.Materialize(r.store, ha)
	if err != nil {
		return fmt.Errorf("replaying %s: %w", a, err)
	}
	idxB, err := patches.Materialize(r.store, hb)
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
	for p := range paths {
		newContent := make([]string, len(idxB[p]))
		for i, l := range idxB[p] {
			newContent[i] = l.Content
		}
		ops, _ := patches.Diff(idxA[p], newContent)
		renderDiff(p, idxA[p], ops)
	}
	return nil
}

// renderDiff prints a simple +/- line diff for one path. It isn't grouped
// into unified-diff hunks with surrounding context — a scaffold-quality
// rendering, not a full diff formatter.
func renderDiff(path string, base []patches.Line, ops []patches.LineOp) {
	if len(ops) == 0 {
		return
	}
	baseContent := make(map[string]string, len(base))
	for _, l := range base {
		baseContent[l.ID] = l.Content
	}
	fmt.Printf("--- %s\n+++ %s\n", path, path)
	for _, op := range ops {
		switch op.Kind {
		case patches.OpDelete:
			fmt.Printf("-%s\n", baseContent[op.ID])
		case patches.OpInsert:
			fmt.Printf("+%s\n", op.Content)
		}
	}
}
