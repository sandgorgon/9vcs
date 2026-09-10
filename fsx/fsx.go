// Package fsx is the filesystem seam objstore/patches and repo.Repo
// operate through, so their storage logic works unmodified whether the
// repo root is a real OS directory (osfs) or resolved through a 9sh
// namespace over 9P (p9fs) — see PLAN.md decision #9.
package fsx

import "time"

// Info is what Stat reports. Exists is false, with a nil error,
// whenever the path can't be confirmed present — this matches every
// existing local Stat call site in this codebase (rawStore.has,
// withRefLock's staleness check), which already collapse "not found"
// and "couldn't check" into the same "treat as absent" outcome; base
// 9P2000 has no structured error code to distinguish them anyway (see
// PLAN.md's library facts).
type Info struct {
	Exists  bool
	IsDir   bool
	ModTime time.Time
}

// FS is the filesystem seam.
type FS interface {
	ReadFile(path string) ([]byte, error)
	// ReadDir returns the names of path's children, in no particular
	// order — enough for rawStore.list/resolvePrefix and Repo.ListRefs,
	// which each do their own sorting/filtering already.
	ReadDir(path string) ([]string, error)
	Stat(path string) (Info, error)
	// MkdirAll ensures path exists as a directory, creating it (and any
	// missing parent) if necessary.
	MkdirAll(path string) error
	// Lock creates path as an empty marker file, failing with an error
	// satisfying errors.Is(err, os.ErrExist) if it already exists — true
	// exclusive-create, no content, the mutual-exclusion primitive
	// withRefLock uses. Deliberately separate from Put: a lock file and
	// a content-addressed put want opposite answers to "it's already
	// there" (contention vs. success), and only a lock needs true
	// create-or-fail semantics — its content never matters, so there's
	// no torn-write concern to design around.
	Lock(path string) error
	// Put stores data at path, content-addressed-idempotent style: safe
	// against a concurrent reader ever observing partial content (via a
	// temp-write-then-publish pattern, not a direct in-place write), and
	// tolerant of path already holding the same content — callers only
	// ever construct calls where that's true, having hashed data first.
	Put(path string, data []byte) error
	// WriteAtomic replaces path's content such that a lock-free reader
	// never observes a torn write — refs/HEAD/merge state. No
	// exclusivity guarantee of its own: callers needing compare-and-swap
	// (casWriteRef) already get it from withRefLock before ever calling
	// this.
	WriteAtomic(path string, data []byte) error
	Remove(path string) error
	// Join and Dir mirror this FS's own path convention (filepath for
	// osfs, path for p9fs — a namespace has no OS-native drive letters
	// or separators) — so callers that build/walk paths generically,
	// like repo.FindAt's walk-up-for-.9vcs loop, don't need to know
	// which backend they're talking to.
	Join(elem ...string) string
	Dir(path string) string
	// IsLocal reports whether this FS is backed by the real OS
	// filesystem (osfs) as opposed to a 9P connection (p9fs). Working-
	// tree materialization (repo.WriteSidecarFile/WorkingFiles) checks
	// this before ever calling os.OpenRoot directly on a Repo's Root
	// string — that string is a real OS path only for osfs; for p9fs
	// it's a namespace path, and blindly handing it to an OS call risks
	// silently operating on an unrelated real directory that happens to
	// share the same relative path string. See PLAN.md decision #9.
	IsLocal() bool
}
