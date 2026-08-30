package main

import (
	"flag"
	"fmt"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

// cmdStatus prints a one-line-per-path summary of what changedFiles would
// record — "what's dirty," as distinct from `diff`'s "what changed, in
// detail." There's only one bucket of change to report (added, modified,
// unmerged, or deleted) since this design has no staging index at all
// (PLAN.md decision #2) — no separate staged/unstaged/untracked split the
// way git's status has, because there's no separate staging step to have
// diverged from the working tree in the first place.
func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return fmt.Errorf("status: expected no arguments")
	}

	r, err := repo.Find()
	if err != nil {
		return err
	}
	branch, err := r.CurrentBranch()
	if err != nil {
		return fmt.Errorf("reading HEAD: %w", err)
	}
	head, _, err := r.HeadHash()
	if err != nil {
		return fmt.Errorf("reading head: %w", err)
	}
	mergeHeads, err := r.MergeHeads()
	if err != nil {
		return fmt.Errorf("reading merge state: %w", err)
	}

	// Same "what am I actually comparing the working tree against" logic
	// record.go's midMerge branch uses: a raw materialize(head) would
	// disagree with what's actually on disk for a path that lost a
	// modify/delete race, misreporting it as dirty when it isn't.
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

	if branch != "" {
		fmt.Printf("on branch %s\n", branch)
	} else {
		fmt.Println("HEAD detached")
	}
	if len(mergeHeads) > 0 {
		fmt.Println("merge in progress; resolve conflicts, then run `9vcs record`")
	}
	if len(changes) == 0 {
		fmt.Println("nothing to record, working tree clean")
		return nil
	}
	for _, p := range repo.SortedPaths(changes) {
		fmt.Printf("%s  %s\n", statusLabel(changes[p], base), p)
	}
	return nil
}

// statusLabel is git-status-short-flavored: A (new path, not in base), D
// (deleted), U (still has an unresolved conflict marker — the same check
// record.go itself uses to refuse recording one), M (anything else).
func statusLabel(fc patches.FileChange, base patches.Index) string {
	if fc.Kind == patches.KindDelete {
		return "D"
	}
	if _, existed := base[fc.Path]; !existed {
		return "A"
	}
	if fc.Kind == patches.KindText {
		for _, op := range fc.Ops {
			if op.Kind == patches.OpInsert && patches.IsMarker(op.Content) {
				return "U"
			}
		}
	}
	return "M"
}
