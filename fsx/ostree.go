package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// osTree is Tree backed by the real OS filesystem, confined to root
// via os.Root — the same primitive, and the same defense, that
// repo.WriteWorkingTree/WriteSidecarFile already used directly before
// this existed: os.Root follows a symlink that stays within root, but
// refuses one (or a path) that would leave it, closing the
// intermediate-symlink escape a plain filepath.Join+os.* call can't
// see coming (see Tree's doc comment).
type osTree struct {
	root *os.Root
}

// NewOSTree opens root (which must already exist) confined via
// os.Root.
func NewOSTree(root string) (Tree, error) {
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	return &osTree{root: r}, nil
}

// path maps a repo-relative path (possibly "", meaning the tree root
// itself) onto what os.Root's own methods expect for that: "." for
// the root — an empty string isn't accepted (confirmed live: an empty
// name fails with "empty path").
func (t *osTree) path(p string) string {
	if p == "" {
		return "."
	}
	return filepath.FromSlash(p)
}

func infoFrom(fi fs.FileInfo, target string) TreeInfo {
	mode := fi.Mode()
	return TreeInfo{
		Exists:        true,
		IsDir:         mode.IsDir(),
		IsSymlink:     mode&os.ModeSymlink != 0,
		SymlinkTarget: target,
		Executable:    mode.IsRegular() && mode&0o111 != 0,
	}
}

func (t *osTree) Lstat(p string) (TreeInfo, error) {
	fi, err := t.root.Lstat(t.path(p))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TreeInfo{}, nil
		}
		return TreeInfo{}, err
	}
	var target string
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err = t.root.Readlink(t.path(p))
		if err != nil {
			return TreeInfo{}, err
		}
	}
	return infoFrom(fi, target), nil
}

func (t *osTree) ReadDir(p string) ([]TreeDirEntry, error) {
	dir, err := t.root.Open(t.path(p))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make([]TreeDirEntry, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		var target string
		if fi.Mode()&os.ModeSymlink != 0 {
			childPath := t.path(p) + string(filepath.Separator) + e.Name()
			target, err = t.root.Readlink(childPath)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, TreeDirEntry{Name: e.Name(), Info: infoFrom(fi, target)})
	}
	return out, nil
}

func (t *osTree) ReadFile(p string) ([]byte, error) { return t.root.ReadFile(t.path(p)) }

// WriteFile creates or replaces p, then explicitly chmods it — POSIX
// open(2)'s mode argument only applies when it actually creates the
// file, so an existing path (checkout overwriting what's already
// there, the common case) needs the follow-up Chmod or a toggled
// executable bit would silently fail to take effect.
func (t *osTree) WriteFile(p string, data []byte, executable bool) error {
	rel := t.path(p)
	if err := t.root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if executable {
		mode = 0o755
	}
	if err := t.root.WriteFile(rel, data, mode); err != nil {
		return err
	}
	return t.root.Chmod(rel, mode)
}

// Symlink clears whatever currently sits at p first — Root.Symlink
// fails outright if a regular file (or a stale symlink to something
// else) is already there, matching WriteWorkingTree's existing
// pre-this-package behavior.
func (t *osTree) Symlink(p, target string) error {
	rel := t.path(p)
	if err := t.root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	if err := t.root.Remove(rel); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return t.root.Symlink(target, rel)
}

func (t *osTree) MkdirAll(p string) error { return t.root.MkdirAll(t.path(p), 0o755) }

func (t *osTree) Remove(p string) error {
	err := t.root.Remove(t.path(p))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (t *osTree) IsLocal() bool { return true }
