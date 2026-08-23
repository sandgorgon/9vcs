package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func cmdInit(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("init takes no arguments")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	dir := filepath.Join(cwd, dotDir)
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists", dir)
	}
	if err := os.MkdirAll(filepath.Join(dir, "refs"), 0o755); err != nil {
		return err
	}
	if _, err := patches.Open(filepath.Join(dir, "patches")); err != nil {
		return err
	}
	fmt.Printf("initialized empty 9vcs repository in %s\n", dir)
	return nil
}
