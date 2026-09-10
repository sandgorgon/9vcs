package fsx

import (
	"errors"
	"os"
	"path/filepath"
)

// osfs is FS backed directly by the local OS filesystem, rooted at
// root. Behaviorally identical to the inline os/filepath calls it
// replaces throughout objstore/patches and repo — a refactor, not a
// behavior change, for the local case.
type osfs struct {
	root string
}

// NewOS returns an FS backed by the real OS filesystem, rooted at root.
func NewOS(root string) FS { return &osfs{root: root} }

func (f *osfs) path(p string) string { return filepath.Join(f.root, filepath.FromSlash(p)) }

func (f *osfs) ReadFile(p string) ([]byte, error) { return os.ReadFile(f.path(p)) }

func (f *osfs) ReadDir(p string) ([]string, error) {
	entries, err := os.ReadDir(f.path(p))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names, nil
}

func (f *osfs) Stat(p string) (Info, error) {
	info, err := os.Stat(f.path(p))
	if err != nil {
		return Info{}, nil // see Info's doc comment: any failure means "absent"
	}
	return Info{Exists: true, IsDir: info.IsDir(), ModTime: info.ModTime()}, nil
}

func (f *osfs) MkdirAll(p string) error { return os.MkdirAll(f.path(p), 0o755) }

// Lock creates p as an empty marker file, true O_EXCL: fails with an
// error satisfying errors.Is(err, os.ErrExist) if it's already there.
func (f *osfs) Lock(p string) error {
	path := f.path(p)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

// Put uses a unique-per-call temp name, write, chmod read-only, then
// hard-links it into place — not a plain O_EXCL open at the final path
// — specifically so a concurrent reader of an in-progress write never
// observes a torn file: the final path only ever appears, via the
// link, once the content is fully written (see PLAN.md decision #9's
// Gap 1 for the live bug this was originally fixed for). Link, not
// rename, so a second call for the same content fails cleanly on
// EEXIST (returned as-is, wrapping os.ErrExist) instead of silently
// re-publishing over an existing link — it's the caller's job
// (rawStore.put) to tolerate that as success, since content-addressed
// callers only ever construct calls where that's true.
func (f *osfs) Put(p string, data []byte) error {
	path := f.path(p)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(data)
	closeErr := tmp.Close()
	if writeErr != nil {
		os.Remove(tmpName)
		return writeErr
	}
	if closeErr != nil {
		os.Remove(tmpName)
		return closeErr
	}
	if err := os.Chmod(tmpName, 0o444); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Link(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	os.Remove(tmpName)
	return nil
}

// WriteAtomic mirrors repo.go's atomicWriteFile: temp file in the same
// directory, then rename into place, so a reader never observes a
// partially-written file.
func (f *osfs) WriteAtomic(p string, data []byte) error {
	path := f.path(p)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Remove is idempotent — a no-op, not an error, if p was never there.
func (f *osfs) Remove(p string) error {
	err := os.Remove(f.path(p))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (f *osfs) Join(elem ...string) string { return filepath.Join(elem...) }
func (f *osfs) Dir(p string) string        { return filepath.Dir(p) }
func (f *osfs) IsLocal() bool              { return true }
