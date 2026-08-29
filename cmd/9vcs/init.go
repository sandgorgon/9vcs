package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sandgorgon/9vcs/repo"
)

func cmdInit(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("init takes no arguments")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	dir := filepath.Join(cwd, repo.DotDir)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o755); err != nil {
		return err
	}

	r, err := repo.Open(cwd)
	if err != nil {
		return err
	}
	if err := r.SetHeadBranch(repo.DefaultBranch); err != nil {
		return err
	}

	fmt.Printf("initialized empty 9vcs repository in %s\n", dir)
	return nil
}
