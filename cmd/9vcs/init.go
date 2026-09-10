package main

import (
	"fmt"

	"github.com/sandgorgon/9vcs/repo"
)

func cmdInit(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("init takes no arguments")
	}
	fs, dir, err := resolveRoot()
	if err != nil {
		return err
	}
	dotDir := fs.Join(dir, repo.DotDir)
	if info, err := fs.Stat(dotDir); err == nil && info.Exists {
		return fmt.Errorf("%s already exists", dotDir)
	}
	// A missing directory over a namespace-resolved (p9fs) root fails
	// here with a clear error naming the blocking 9p issue — see
	// PLAN.md decision #9 and fsx.ErrWriteUnsupported's own message.
	if err := fs.MkdirAll(fs.Join(dotDir, "refs")); err != nil {
		return err
	}

	r, err := repo.OpenFS(fs, dir)
	if err != nil {
		return err
	}
	if err := r.SetHeadBranch(repo.DefaultBranch); err != nil {
		return err
	}

	fmt.Printf("initialized empty 9vcs repository in %s\n", dotDir)
	return nil
}
