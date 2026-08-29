package main

import (
	"flag"
	"fmt"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

func cmdCheckout(args []string) error {
	fs := flag.NewFlagSet("checkout", flag.ExitOnError)
	create := fs.Bool("b", false, "create the branch, starting from the given point, then check it out")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	maxArgs := 1
	if *create {
		maxArgs = 2 // <name> [<start-point>]
	}
	if len(rest) < 1 || len(rest) > maxArgs {
		if *create {
			return fmt.Errorf("checkout -b: expected <name> [<start-point>]")
		}
		return fmt.Errorf("checkout: expected exactly one branch name or patch hash")
	}
	name := rest[0]

	r, err := repo.Find()
	if err != nil {
		return err
	}

	oldHead, _, err := r.HeadHash()
	if err != nil {
		return fmt.Errorf("reading head: %w", err)
	}

	// Refuse to clobber uncommitted work, same as git's real checkout safety check.
	base, err := r.Materialize(oldHead)
	if err != nil {
		return fmt.Errorf("replaying current history: %w", err)
	}
	dirty, err := repo.ChangedFiles(r, base)
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
		return fmt.Errorf("checkout: uncommitted changes would be overwritten; record or discard them first")
	}

	var (
		targetHash patches.Hash
		branch     string // "" means detached
	)
	switch {
	case *create:
		if _, ok, err := r.RefHash(name); err != nil {
			return err
		} else if ok {
			return fmt.Errorf("branch %q already exists", name)
		}
		start := oldHead
		if len(rest) == 2 {
			start, err = r.ResolveRef(rest[1])
			if err != nil {
				return fmt.Errorf("checkout -b: %w", err)
			}
		}
		if err := r.SetLocalRefCAS(name, patches.Hash{}, start); err != nil {
			return err
		}
		targetHash, branch = start, name
	default:
		if h, ok, err := r.RefHash(name); err != nil {
			return err
		} else if ok {
			targetHash, branch = h, name
			break
		}
		h, err := r.Store.ResolvePrefix(name)
		if err != nil {
			return fmt.Errorf("checkout: unknown branch or patch %q", name)
		}
		targetHash, branch = h, ""
	}

	target, err := r.Materialize(targetHash)
	if err != nil {
		return fmt.Errorf("replaying target history: %w", err)
	}
	if err := repo.WriteWorkingTree(r, base, target); err != nil {
		return fmt.Errorf("writing working tree: %w", err)
	}

	if branch != "" {
		if err := r.SetHeadBranch(branch); err != nil {
			return err
		}
		fmt.Printf("switched to branch %s\n", branch)
	} else {
		if err := r.SetHeadDetached(targetHash); err != nil {
			return err
		}
		fmt.Printf("checked out %s (detached)\n", targetHash.String()[:12])
	}
	return nil
}
