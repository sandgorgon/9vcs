package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func cmdRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	message := fs.String("m", "", "patch message")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *message == "" {
		return fmt.Errorf("record: -m MESSAGE is required")
	}

	r, err := findRepo()
	if err != nil {
		return err
	}

	branch, err := r.currentBranch()
	if err != nil {
		return fmt.Errorf("reading HEAD: %w", err)
	}
	if branch == "" {
		return fmt.Errorf("record: HEAD is detached; check out a branch first")
	}

	head, _, err := r.headHash()
	if err != nil {
		return fmt.Errorf("reading head: %w", err)
	}
	mergeHead, midMerge, err := r.mergeHead()
	if err != nil {
		return fmt.Errorf("reading merge state: %w", err)
	}
	if midMerge {
		// Remove merge's comparison sidecars before diffing the working
		// tree — they're tooling, not content, and must never end up
		// picked up as a newly-tracked file in the merge's own patch.
		sidecars, err := r.mergeSidecars()
		if err != nil {
			return fmt.Errorf("reading merge state: %w", err)
		}
		for _, s := range sidecars {
			full := filepath.Join(r.root, filepath.FromSlash(s))
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("removing %s: %w", s, err)
			}
		}
	}

	var base patches.Index
	var mergeConflicts []mergeConflict
	if midMerge {
		// The same resolution merge used to decide what to write to the
		// working tree, not a raw Materialize — a raw union wouldn't
		// know to prefer a modified path over one that lost a
		// modify/delete race, and would disagree with what's actually on
		// disk for that path, corrupting line identity on the next edit.
		base, mergeConflicts, err = computeMerge(r, head, mergeHead)
	} else {
		base, err = r.materialize(head)
	}
	if err != nil {
		return fmt.Errorf("replaying history: %w", err)
	}

	changes, err := changedFiles(r, base)
	if err != nil {
		return err
	}

	// A modify/delete conflict resolved by keeping the content (as
	// opposed to honoring the deletion, which changedFiles already
	// handles fine — a delete is unambiguous regardless of graph state)
	// needs an explicit, self-contained FileChange here, even when
	// nothing textually differs from base. Without one, nothing in this
	// patch actually pins the outcome: a future replay still has to pick
	// between the deleting patch and the modifying patch by the same
	// deterministic topological tiebreak that created the conflict, and
	// there's no guarantee it resolves the way base (and the working
	// tree, right now) happens to show. A fresh full insert is the only
	// form that's correct regardless of that tiebreak — it doesn't
	// depend on which graph object survived replay to diff against.
	for _, c := range mergeConflicts {
		if c.Kind != "modify/delete" {
			continue
		}
		full := filepath.Join(r.root, filepath.FromSlash(c.Path))
		content, err := os.ReadFile(full)
		if os.IsNotExist(err) {
			continue // honoring the deletion; changedFiles' KindDelete already covers it
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", c.Path, err)
		}
		if isBinary(content) {
			hash, err := r.blobs.Put(content)
			if err != nil {
				return fmt.Errorf("storing blob for %s: %w", c.Path, err)
			}
			changes[c.Path] = patches.FileChange{Path: c.Path, Kind: patches.KindBlob, Blob: hash}
			continue
		}
		ops, _ := patches.Diff(nil, splitLines(string(content)))
		changes[c.Path] = patches.FileChange{Path: c.Path, Kind: patches.KindText, Ops: ops, TrailingNewline: hasTrailingNewline(content)}
	}

	if len(changes) == 0 && !midMerge {
		fmt.Println("nothing to record")
		return nil
	}
	for _, fc := range changes {
		if fc.Kind != patches.KindText {
			continue
		}
		for _, op := range fc.Ops {
			if op.Kind == patches.OpInsert && patches.IsMarker(op.Content) {
				return fmt.Errorf("%s: still has unresolved conflict markers; resolve them before recording", fc.Path)
			}
		}
	}

	var deps []patches.Hash
	if !head.IsZero() {
		deps = append(deps, head)
	}
	if midMerge && !mergeHead.IsZero() {
		deps = append(deps, mergeHead)
	}

	authorStr, err := author(r)
	if err != nil {
		return fmt.Errorf("record: %w", err)
	}
	patch := &patches.Patch{Dependencies: deps, Author: authorStr, Time: time.Now(), Message: *message}
	for _, fc := range changes {
		patch.Changes = append(patch.Changes, fc)
	}
	signPatch(patch)

	hash, err := r.store.Put(patch)
	if err != nil {
		return fmt.Errorf("writing patch: %w", err)
	}
	if err := r.setRefHash(branch, hash); err != nil {
		return fmt.Errorf("updating ref: %w", err)
	}
	if midMerge {
		if err := r.clearMergeHead(); err != nil {
			return fmt.Errorf("clearing merge state: %w", err)
		}
		if err := r.clearMergeSidecars(); err != nil {
			return fmt.Errorf("clearing merge state: %w", err)
		}
	}

	fmt.Printf("recorded %s: %s\n", hash.String()[:12], *message)
	return nil
}
