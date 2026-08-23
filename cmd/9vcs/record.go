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
	if midMerge {
		base, err = patches.Materialize(r.store, head, mergeHead)
	} else {
		base, err = patches.Materialize(r.store, head)
	}
	if err != nil {
		return fmt.Errorf("replaying history: %w", err)
	}

	changes, err := changedFiles(r, base)
	if err != nil {
		return err
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

	patch := &patches.Patch{Dependencies: deps, Author: author(), Time: time.Now(), Message: *message}
	for _, fc := range changes {
		patch.Changes = append(patch.Changes, fc)
	}

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
