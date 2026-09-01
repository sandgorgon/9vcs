package main

import (
	"flag"
	"fmt"
	"sort"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

// cmdRestore discards uncommitted changes to specific paths, writing each
// one back to its state at head (or, mid-merge, at the merged base — same
// "what am I comparing against" logic status.go and record.go's midMerge
// branch use). Unlike checkout, which is whole-tree and refuses outright
// if anything is dirty, restore is scoped to exactly the paths named and
// exists to discard dirty state, not guard against clobbering it.
//
// A path with no entry in base (an uncommitted addition, or one half of
// an uncommitted rename) restores to "doesn't exist" — deleted — rather
// than erroring, since 9vcs has no staging index (PLAN.md decision #2):
// there's no distinct "untracked" state to protect the way git's does,
// every non-ignored working-tree path is implicitly a pending change.
// That also makes reverting a rename just naming both paths: the new
// name deletes (no base entry) and the old name is rewritten from base.
func cmdRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths := fs.Args()
	if len(paths) == 0 {
		return fmt.Errorf("restore: expected one or more paths")
	}

	r, err := repo.Find()
	if err != nil {
		return err
	}
	return restorePaths(r, paths)
}

// restorePaths is cmdRestore's core, taking r directly rather than going
// through repo.Find() — same split merge.go uses for cmdMergeAbort, so
// tests can drive it against a repo.Repo without depending on cwd.
func restorePaths(r *repo.Repo, paths []string) error {
	head, _, err := r.HeadHash()
	if err != nil {
		return fmt.Errorf("reading head: %w", err)
	}
	mergeHeads, err := r.MergeHeads()
	if err != nil {
		return fmt.Errorf("reading merge state: %w", err)
	}

	var base patches.Index
	if len(mergeHeads) > 0 {
		base, _, err = computeMerge(r, append([]patches.Hash{head}, mergeHeads...)...)
	} else {
		base, err = r.Materialize(head)
	}
	if err != nil {
		return fmt.Errorf("replaying history: %w", err)
	}

	changes, err := repo.ChangedFiles(r, base)
	if err != nil {
		return err
	}

	old := patches.Index{}
	target := patches.Index{}
	var restored []string
	for _, p := range paths {
		_, dirty := changes[p]
		st, tracked := base[p]
		if !dirty {
			if !tracked {
				return fmt.Errorf("restore: %q has no recorded history and no uncommitted changes", p)
			}
			continue // already matches recorded state
		}
		old[p] = patches.PathState{}
		if tracked {
			target[p] = st
		}
		restored = append(restored, p)
	}

	if err := repo.WriteWorkingTree(r, old, target); err != nil {
		return fmt.Errorf("restoring: %w", err)
	}

	sort.Strings(restored)
	for _, p := range restored {
		if _, ok := target[p]; ok {
			fmt.Printf("restored %s\n", p)
		} else {
			fmt.Printf("removed %s\n", p)
		}
	}
	return nil
}
