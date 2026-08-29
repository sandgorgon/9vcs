package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

func cmdMerge(args []string) error {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	abort := fs.Bool("abort", false, "abandon the merge in progress (from merge or apply) and restore the working tree to head")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()

	if *abort {
		if len(rest) != 0 {
			return fmt.Errorf("merge -abort: no other arguments expected")
		}
		r, err := repo.Find()
		if err != nil {
			return err
		}
		return cmdMergeAbort(r)
	}

	if len(rest) != 1 {
		return fmt.Errorf("merge: expected exactly one branch name or patch hash")
	}

	r, err := repo.Find()
	if err != nil {
		return err
	}

	if heads, err := r.MergeHeads(); err != nil {
		return err
	} else if len(heads) > 0 {
		return fmt.Errorf("merge: a merge is already in progress; resolve conflicts and run record, or `9vcs merge -abort` to abandon it")
	}

	branch, err := r.CurrentBranch()
	if err != nil {
		return fmt.Errorf("reading HEAD: %w", err)
	}
	if branch == "" {
		return fmt.Errorf("merge: HEAD is detached; check out a branch first")
	}

	ours, _, err := r.HeadHash()
	if err != nil {
		return fmt.Errorf("reading head: %w", err)
	}
	theirs, err := r.ResolveRef(rest[0])
	if err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	if ours == theirs {
		fmt.Println("already up to date")
		return nil
	}

	oursClosure, err := patches.Closure(r.Store, ours)
	if err != nil {
		return fmt.Errorf("replaying current history: %w", err)
	}
	if oursClosure[theirs] {
		fmt.Println("already up to date")
		return nil
	}

	oursIdx, err := r.Materialize(ours)
	if err != nil {
		return fmt.Errorf("replaying current history: %w", err)
	}
	dirty, err := repo.ChangedFiles(r, oursIdx)
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf("merge: uncommitted changes would be overwritten; record or discard them first")
	}

	theirsClosure, err := patches.Closure(r.Store, theirs)
	if err != nil {
		return fmt.Errorf("replaying %s: %w", rest[0], err)
	}
	if theirsClosure[ours] {
		// Fast-forward: our own history is already a prefix of theirs.
		theirsIdx, err := r.Materialize(theirs)
		if err != nil {
			return fmt.Errorf("replaying %s: %w", rest[0], err)
		}
		if err := repo.WriteWorkingTree(r, oursIdx, theirsIdx); err != nil {
			return fmt.Errorf("writing working tree: %w", err)
		}
		if err := r.SetLocalRefCAS(branch, ours, theirs); err != nil {
			return err
		}
		fmt.Printf("fast-forwarded %s to %s\n", branch, theirs.String()[:12])
		return nil
	}

	merged, conflicts, err := computeMerge(r, ours, theirs)
	if err != nil {
		return err
	}

	// Binary conflicts get theirs' content written alongside as a
	// comparison sidecar (e.g. "logo.png.theirs" next to "logo.png", which
	// computeMerge already resolved to keep ours) — record deletes it once
	// the merge is finalized, it's not tracked content.
	var sidecars []string
	var theirsIdx patches.Index
	for _, c := range conflicts {
		if c.Kind != "binary" {
			continue
		}
		if theirsIdx == nil {
			theirsIdx, err = r.Materialize(theirs)
			if err != nil {
				return fmt.Errorf("replaying %s: %w", rest[0], err)
			}
		}
		data, err := r.Blobs.Get(theirsIdx[c.Path].Blob)
		if err != nil {
			return fmt.Errorf("reading blob for %s: %w", c.Path, err)
		}
		sidecar := repo.BinaryConflictSidecar(c.Path, theirs)
		if err := repo.WriteSidecarFile(r, sidecar, data); err != nil {
			return fmt.Errorf("writing %s: %w", sidecar, err)
		}
		sidecars = append(sidecars, sidecar)
	}

	if err := repo.WriteWorkingTree(r, oursIdx, merged); err != nil {
		return fmt.Errorf("writing working tree: %w", err)
	}
	if err := r.SetMergeHeads([]patches.Hash{theirs}); err != nil {
		return fmt.Errorf("recording merge state: %w", err)
	}
	if err := r.SetMergeSidecars(sidecars); err != nil {
		return fmt.Errorf("recording merge state: %w", err)
	}

	if len(conflicts) == 0 {
		fmt.Println("merged cleanly; run `9vcs record` to finish")
		return nil
	}
	fmt.Println("automatic merge failed; fix conflicts, then run `9vcs record` to finish:")
	for _, c := range conflicts {
		switch c.Kind {
		case "text":
			fmt.Printf("  CONFLICT (text): %s — resolve the <<<<<<< / ======= / >>>>>>> markers in the file, then record\n", c.Path)
		case "binary":
			fmt.Printf("  CONFLICT (binary): %s — kept your version; theirs is at %s for comparison\n", c.Path, repo.BinaryConflictSidecar(c.Path, theirs))
		case "modify/delete":
			fmt.Printf("  CONFLICT (modify/delete): %s — deleted by %s, modified by the other side; kept the modified version\n", c.Path, c.DeletedBy)
		case "symlink":
			fmt.Printf("  CONFLICT (symlink): %s — kept your target; other target(s): %s\n", c.Path, strings.Join(c.OtherTargets, ", "))
		case "type":
			fmt.Printf("  CONFLICT (type): %s — this path is a different kind of thing (text/binary/symlink) on each side; kept your version\n", c.Path)
		default:
			fmt.Printf("  CONFLICT: %s\n", c.Path)
		}
	}
	return nil
}

// cmdMergeAbort abandons whatever merge is in progress — whether started
// by `merge` or `apply`, both of which write the exact same MERGE_HEAD/
// MERGE_SIDECARS state (see PLAN.md decision #8's "apply — concrete
// scope": apply's N-way MERGE_HEAD generalization is what makes one
// abort implementation correct for both, with no special-casing of which
// command started it). Recomputes exactly what was written to the
// working tree (the same deterministic computeMerge(head, mergeHeads...)
// call merge/apply/status all already make — a pure function of its
// roots, so this reproduces it byte-for-byte without needing to have
// remembered it), then reverses it: writeWorkingTree(r, merged, headIdx)
// overwrites every path back to head's content and removes anything the
// incoming side added, exactly mirroring how a plain checkout/pullRef
// replaces one materialized state with another.
//
// Like `git merge --abort`, this discards any hand-editing done to
// resolve conflicts since the merge started — there's no partial-save;
// it's a full return to head, not a selective undo.
func cmdMergeAbort(r *repo.Repo) error {
	heads, err := r.MergeHeads()
	if err != nil {
		return fmt.Errorf("merge -abort: %w", err)
	}
	if len(heads) == 0 {
		return fmt.Errorf("merge -abort: no merge in progress")
	}

	head, _, err := r.HeadHash()
	if err != nil {
		return fmt.Errorf("reading head: %w", err)
	}
	headIdx, err := r.Materialize(head)
	if err != nil {
		return fmt.Errorf("replaying history: %w", err)
	}
	merged, _, err := computeMerge(r, append([]patches.Hash{head}, heads...)...)
	if err != nil {
		return fmt.Errorf("merge -abort: %w", err)
	}
	if err := repo.WriteWorkingTree(r, merged, headIdx); err != nil {
		return fmt.Errorf("merge -abort: restoring working tree: %w", err)
	}

	sidecars, err := r.MergeSidecars()
	if err != nil {
		return fmt.Errorf("merge -abort: %w", err)
	}
	for _, s := range sidecars {
		if err := repo.RemoveSidecarFile(r, s); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("merge -abort: removing %s: %w", s, err)
		}
	}

	if err := r.ClearMergeHeads(); err != nil {
		return fmt.Errorf("merge -abort: %w", err)
	}
	if err := r.ClearMergeSidecars(); err != nil {
		return fmt.Errorf("merge -abort: %w", err)
	}

	fmt.Printf("merge aborted; working tree restored to %s\n", head.String()[:12])
	return nil
}
