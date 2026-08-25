package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

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

	if heads, err := r.mergeHeads(); err != nil {
		return err
	} else if len(heads) > 0 {
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

	oursIdx, err := r.materialize(ours)
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

	theirsClosure, err := patches.Closure(r.store, theirs)
	if err != nil {
		return fmt.Errorf("replaying %s: %w", rest[0], err)
	}
	if theirsClosure[ours] {
		// Fast-forward: our own history is already a prefix of theirs.
		theirsIdx, err := r.materialize(theirs)
		if err != nil {
			return fmt.Errorf("replaying %s: %w", rest[0], err)
		}
		if err := writeWorkingTree(r, oursIdx, theirsIdx); err != nil {
			return fmt.Errorf("writing working tree: %w", err)
		}
		if err := r.setRefHash(branch, theirs); err != nil {
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
			theirsIdx, err = r.materialize(theirs)
			if err != nil {
				return fmt.Errorf("replaying %s: %w", rest[0], err)
			}
		}
		data, err := r.blobs.Get(theirsIdx[c.Path].Blob)
		if err != nil {
			return fmt.Errorf("reading blob for %s: %w", c.Path, err)
		}
		sidecar := binaryConflictSidecar(c.Path, theirs)
		full := filepath.Join(r.root, filepath.FromSlash(sidecar))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", sidecar, err)
		}
		sidecars = append(sidecars, sidecar)
	}

	if err := writeWorkingTree(r, oursIdx, merged); err != nil {
		return fmt.Errorf("writing working tree: %w", err)
	}
	if err := r.setMergeHeads([]patches.Hash{theirs}); err != nil {
		return fmt.Errorf("recording merge state: %w", err)
	}
	if err := r.setMergeSidecars(sidecars); err != nil {
		return fmt.Errorf("recording merge state: %w", err)
	}

	if len(conflicts) == 0 {
		fmt.Println("merged cleanly; run `9vcs record` to finish")
		return nil
	}
	fmt.Println("automatic merge failed; fix conflicts, then run `9vcs record` to finish:")
	for _, c := range conflicts {
		switch c.Kind {
		case "binary":
			fmt.Printf("  CONFLICT (binary): %s — kept your version; theirs is at %s for comparison\n", c.Path, binaryConflictSidecar(c.Path, theirs))
		case "modify/delete":
			fmt.Printf("  CONFLICT (modify/delete): %s — deleted by %s, modified by the other side; kept the modified version\n", c.Path, c.DeletedBy)
		default:
			fmt.Printf("  CONFLICT: %s\n", c.Path)
		}
	}
	return nil
}
