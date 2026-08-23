package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// dotDir is the on-disk root for all local repo state, sibling to .git.
const dotDir = ".9vcs"

// defaultBranch is the branch `init` points HEAD at.
const defaultBranch = "main"

// repo resolves the paths and stores for one 9vcs repository.
type repo struct {
	root  string // working tree root (parent of .9vcs)
	dir   string // .9vcs
	store *patches.Store
	blobs *patches.BlobStore
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
	blobs, err := patches.OpenBlobs(filepath.Join(dir, "blobs"))
	if err != nil {
		return nil, err
	}
	return &repo{root: root, dir: dir, store: store, blobs: blobs}, nil
}

func (r *repo) headFile() string { return filepath.Join(r.dir, "HEAD") }

func (r *repo) refPath(name string) string { return filepath.Join(r.dir, "refs", name) }

// currentBranch returns the branch name HEAD points to, or "" if HEAD is
// detached (points directly at a patch hash instead of a branch name).
func (r *repo) currentBranch() (string, error) {
	data, err := os.ReadFile(r.headFile())
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	if branch, ok := strings.CutPrefix(s, "ref: "); ok {
		return branch, nil
	}
	return "", nil
}

func (r *repo) setHeadBranch(name string) error {
	return os.WriteFile(r.headFile(), []byte("ref: "+name+"\n"), 0o644)
}

func (r *repo) setHeadDetached(h patches.Hash) error {
	return os.WriteFile(r.headFile(), []byte(h.String()+"\n"), 0o644)
}

// headHash resolves HEAD (symbolic or detached) to a concrete patch hash.
// ok is false only when HEAD is a branch that has no patches recorded yet.
func (r *repo) headHash() (patches.Hash, bool, error) {
	branch, err := r.currentBranch()
	if err != nil {
		return patches.Hash{}, false, err
	}
	if branch == "" {
		data, err := os.ReadFile(r.headFile())
		if err != nil {
			return patches.Hash{}, false, err
		}
		h, err := patches.HashFromHex(strings.TrimSpace(string(data)))
		return h, true, err
	}
	return r.refHash(branch)
}

// refHash reads the head patch hash of branch name, if it has one yet.
func (r *repo) refHash(name string) (patches.Hash, bool, error) {
	data, err := os.ReadFile(r.refPath(name))
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

func (r *repo) setRefHash(name string, h patches.Hash) error {
	if err := os.MkdirAll(filepath.Dir(r.refPath(name)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(r.refPath(name), []byte(h.String()+"\n"), 0o644)
}

// listBranches returns every branch name with a ref file, sorted.
func (r *repo) listBranches() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(r.dir, "refs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// resolveRef resolves arg to a patch hash: an exact branch name first, then
// a full or abbreviated patch hash.
func (r *repo) resolveRef(arg string) (patches.Hash, error) {
	if h, ok, err := r.refHash(arg); err != nil {
		return patches.Hash{}, err
	} else if ok {
		return h, nil
	}
	h, err := r.store.ResolvePrefix(arg)
	if err != nil {
		return patches.Hash{}, err
	}
	return h, nil
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
