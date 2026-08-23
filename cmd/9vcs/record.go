package main

import (
	"flag"
	"fmt"
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
	base, err := patches.Materialize(r.store, head)
	if err != nil {
		return fmt.Errorf("replaying history: %w", err)
	}

	changes, err := changedFiles(r, base)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Println("nothing to record")
		return nil
	}

	patch := &patches.Patch{Parent: head, Author: author(), Time: time.Now(), Message: *message}
	for p, ops := range changes {
		patch.Changes = append(patch.Changes, patches.FileChange{Path: p, Ops: ops})
	}

	hash, err := r.store.Put(patch)
	if err != nil {
		return fmt.Errorf("writing patch: %w", err)
	}
	if err := r.setRefHash(branch, hash); err != nil {
		return fmt.Errorf("updating ref: %w", err)
	}

	fmt.Printf("recorded %s: %s\n", hash.String()[:12], *message)
	return nil
}
