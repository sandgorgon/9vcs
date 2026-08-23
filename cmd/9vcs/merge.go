package main

import (
	"flag"
	"fmt"
	"sort"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func cmdMerge(args []string) error {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("merge: expected exactly one branch name or patch hash")
	}

	r, err := findRepo()
	if err != nil {
		return err
	}

	if _, mid, err := r.mergeHead(); err != nil {
		return err
	} else if mid {
		return fmt.Errorf("merge: a merge is already in progress; resolve conflicts and run record, or remove %s to abort", r.mergeHeadFile())
	}

	branch, err := r.currentBranch()
	if err != nil {
		return fmt.Errorf("reading HEAD: %w", err)
	}
	if branch == "" {
		return fmt.Errorf("merge: HEAD is detached; check out a branch first")
	}

	ours, _, err := r.headHash()
	if err != nil {
		return fmt.Errorf("reading head: %w", err)
	}
	theirs, err := r.resolveRef(rest[0])
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	if ours == theirs {
		fmt.Println("already up to date")
		return nil
	}

	oursClosure, err := patches.Closure(r.store, ours)
	if err != nil {
		return fmt.Errorf("replaying current history: %w", err)
	}
	if oursClosure[theirs] {
		fmt.Println("already up to date")
		return nil
	}

	oursIdx, err := patches.Materialize(r.store, ours)
	if err != nil {
		return fmt.Errorf("replaying current history: %w", err)
	}
	dirty, err := changedFiles(r, oursIdx)
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf("merge: uncommitted changes would be overwritten; record or discard them first")
	}

	theirsIdx, err := patches.Materialize(r.store, theirs)
	if err != nil {
		return fmt.Errorf("replaying %s: %w", rest[0], err)
	}
	theirsClosure, err := patches.Closure(r.store, theirs)
	if err != nil {
		return fmt.Errorf("replaying %s: %w", rest[0], err)
	}
	if theirsClosure[ours] {
		// Fast-forward: our own history is already a prefix of theirs.
		if err := writeWorkingTree(r, oursIdx, theirsIdx); err != nil {
			return fmt.Errorf("writing working tree: %w", err)
		}
		if err := r.setRefHash(branch, theirs); err != nil {
			return err
		}
		fmt.Printf("fast-forwarded %s to %s\n", branch, theirs.String()[:12])
		return nil
	}

	merged, err := patches.Materialize(r.store, ours, theirs)
	if err != nil {
		return fmt.Errorf("replaying merged history: %w", err)
	}

	// Binary paths have no line-graph fork mechanism to detect divergence
	// automatically — check directly, and fall back to keeping our own
	// content, same tradeoff git makes for binary conflicts.
	var conflicts []string
	for p, ourSt := range oursIdx {
		if ourSt.Kind != patches.KindBlob {
			continue
		}
		theirSt, ok := theirsIdx[p]
		if !ok || theirSt.Kind != patches.KindBlob || theirSt.Blob == ourSt.Blob {
			continue
		}
		conflicts = append(conflicts, p)
		merged[p] = ourSt // keep ours in the working tree; user resolves by hand
	}
	for p, st := range merged {
		if st.Kind != patches.KindText || st.Graph == nil {
			continue
		}
		if _, forks := patches.Linearize(st.Graph); len(forks) > 0 {
			conflicts = append(conflicts, p)
		}
	}

	if err := writeWorkingTree(r, oursIdx, merged); err != nil {
		return fmt.Errorf("writing working tree: %w", err)
	}
	if err := r.setMergeHead(theirs); err != nil {
		return fmt.Errorf("recording merge state: %w", err)
	}

	if len(conflicts) == 0 {
		fmt.Println("merged cleanly; run `9vcs record` to finish")
		return nil
	}
	sort.Strings(conflicts)
	fmt.Println("automatic merge failed; fix conflicts, then run `9vcs record` to finish:")
	for _, p := range conflicts {
		fmt.Printf("  CONFLICT: %s\n", p)
	}
	return nil
}
