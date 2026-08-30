package main

import (
	"fmt"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

func cmdBranch(args []string) error {
	r, err := repo.Find()
	if err != nil {
		return err
	}

	if len(args) == 0 {
		return listBranches(r)
	}
	if len(args) > 2 {
		return fmt.Errorf("branch: expected [<name> [<start-point>]]")
	}

	name := args[0]
	if _, ok, err := r.RefHash(name); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("branch %q already exists", name)
	}

	var start patches.Hash
	if len(args) == 2 {
		start, err = r.ResolveRef(args[1])
		if err != nil {
			return fmt.Errorf("branch: %w", err)
		}
	} else {
		start, _, err = r.HeadHash()
		if err != nil {
			return fmt.Errorf("reading head: %w", err)
		}
		if start.IsZero() {
			return fmt.Errorf("branch: no patches recorded yet, nothing to branch from")
		}
	}

	if err := r.SetLocalRefCAS(name, patches.Hash{}, start); err != nil {
		return err
	}
	fmt.Printf("created branch %s at %s\n", name, start.String()[:12])
	return nil
}

func listBranches(r *repo.Repo) error {
	names, err := r.ListRefs()
	if err != nil {
		return err
	}
	current, err := r.CurrentBranch()
	if err != nil {
		return err
	}
	for _, n := range names {
		marker := "  "
		if n == current {
			marker = "* "
		}
		fmt.Println(marker + n)
	}
	return nil
}
