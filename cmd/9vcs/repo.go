package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// dotDir is the on-disk root for all local repo state, sibling to .git.
const dotDir = ".9vcs"

// repo resolves the paths and stores for one 9vcs repository.
type repo struct {
	root      string // working tree root (parent of .9vcs)
	dir       string // .9vcs
	store     *patches.Store
	indexPath string
	refPath   string // .9vcs/refs/main — single ref for this scaffold
}

var errNotARepo = errors.New("not a 9vcs repository (or any parent directory)")

// findRepo walks up from the current directory looking for .9vcs, the same
// way git walks up looking for .git.
func findRepo() (*repo, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	dir := cwd
	for {
		candidate := filepath.Join(dir, dotDir)
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return openRepo(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, errNotARepo
		}
		dir = parent
	}
}

func openRepo(root string) (*repo, error) {
	dir := filepath.Join(root, dotDir)
	store, err := patches.Open(filepath.Join(dir, "patches"))
	if err != nil {
		return nil, err
	}
	return &repo{
		root:      root,
		dir:       dir,
		store:     store,
		indexPath: filepath.Join(dir, "index"),
		refPath:   filepath.Join(dir, "refs", "main"),
	}, nil
}

func (r *repo) headHash() (patches.Hash, bool, error) {
	data, err := os.ReadFile(r.refPath)
	if errors.Is(err, os.ErrNotExist) {
		return patches.Hash{}, false, nil
	}
	if err != nil {
		return patches.Hash{}, false, err
	}
	h, err := patches.HashFromHex(strings.TrimSpace(string(data)))
	if err != nil {
		return patches.Hash{}, false, err
	}
	return h, true, nil
}

func (r *repo) setHead(h patches.Hash) error {
	if err := os.MkdirAll(filepath.Dir(r.refPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(r.refPath, []byte(h.String()+"\n"), 0o644)
}

// workingFiles walks the working tree, returning repo-relative paths for
// every regular file outside .9vcs.
func (r *repo) workingFiles() ([]string, error) {
	var out []string
	err := filepath.WalkDir(r.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(r.root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if d.Name() == dotDir {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}
