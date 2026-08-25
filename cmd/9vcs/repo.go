package main

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/synth"
)

// dotDir is the on-disk root for all local repo state, sibling to .git.
const dotDir = ".9vcs"

// defaultBranch is the branch `init` points HEAD at.
const defaultBranch = "main"

// repo resolves the paths and stores for one 9vcs repository.
type repo struct {
	root   string // working tree root (parent of .9vcs)
	dir    string // .9vcs
	store  *patches.Store
	blobs  *patches.BlobStore
	offers *patches.BlobStore // pending offer bundles received via `9vcs serve`'s /offers — see PLAN.md decision #8
	cache  *synth.Cache       // memoizes materialize within this one invocation
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
	offers, err := patches.OpenBlobs(filepath.Join(dir, "offers"))
	if err != nil {
		return nil, err
	}
	return &repo{root: root, dir: dir, store: store, blobs: blobs, offers: offers, cache: synth.NewCache(store)}, nil
}

// materialize is patches.Materialize(r.store, roots...), memoized for
// the lifetime of this repo value (i.e. this one command invocation) —
// see synth.Cache. A single command commonly replays overlapping
// closures more than once (a merge preview materializes ours, theirs,
// and their union all in one call), so every command in this package
// should call this instead of patches.Materialize directly.
func (r *repo) materialize(roots ...patches.Hash) (patches.Index, error) {
	return r.cache.Materialize(roots...)
}

// refLockPath is the cross-process advisory lock every ref/HEAD mutation
// takes for its critical section — see withRefLock.
func (r *repo) refLockPath() string { return filepath.Join(r.dir, "lock") }

const (
	// refLockAcquireTimeout bounds how long withRefLock waits for a
	// contended lock before giving up — generous relative to the
	// critical section it protects (a single small file read + write,
	// milliseconds in practice), so a real timeout here means something
	// is actually stuck, not just briefly busy.
	refLockAcquireTimeout = 5 * time.Second
	refLockRetryInterval  = 20 * time.Millisecond
	// refLockStaleAge is how old an existing lock file has to be before
	// withRefLock assumes it was abandoned by a crashed process and
	// steals it, rather than deadlocking forever. Generous relative to
	// how long the critical section this guards ever legitimately runs
	// for, for the same reason as refLockAcquireTimeout.
	refLockStaleAge = 10 * time.Second
)

// withRefLock runs fn while holding this repo's cross-process file lock
// — the actual mutual exclusion setRefHashCAS/setLocalRefCAS/
// setHeadBranch/setHeadDetached need. An in-memory mutex (what this
// repo used before) only ever guards goroutines within one process; two
// separate local CLI invocations, or a local command racing a live
// `serve`'s incoming push, are different OS processes with no shared
// memory to synchronize through at all. Go's stdlib has no flock
// primitive, and this project stays stdlib-only (see PLAN.md), so this
// uses os.O_EXCL as the actual mutex primitive instead: atomically
// creating the lock file is the acquire, removing it is the release —
// same shape as many tools' simple lockfile convention.
func (r *repo) withRefLock(fn func() error) error {
	path := r.refLockPath()
	deadline := time.Now().Add(refLockAcquireTimeout)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			break
		}
		if !os.IsExist(err) {
			return fmt.Errorf("acquiring ref lock: %w", err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > refLockStaleAge {
			os.Remove(path) // best-effort: if another stealer wins this race, the next loop iteration's OpenFile sorts it out
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ref lock %s is held by another 9vcs process; if you're sure nothing else is running against this repo, remove it manually", path)
		}
		time.Sleep(refLockRetryInterval)
	}
	defer os.Remove(path)
	return fn()
}

// atomicWriteFile writes data to path via a temp file in the same
// directory, then renames it into place — matching
// objstore/patches/rawstore.go's rawStore.put: a reader (or a crash
// mid-write) never observes a partially-written file, only the old
// content or the new content, in full, never a mix. Plain os.WriteFile
// (what every ref/HEAD write used before) offers no such guarantee.
func atomicWriteFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (r *repo) headFile() string { return filepath.Join(r.dir, "HEAD") }

func (r *repo) refPath(name string) string { return filepath.Join(r.dir, "refs", name) }

// validRefName mirrors objstore/patches' FileChange.Path validation —
// same shape, same reason: refPath joins name straight onto r.dir via
// filepath.Join, and nested branch names are a real, intentional feature
// (writeRefFileLocked's MkdirAll), so name can't just be rejected for
// containing "/" — only a ".." segment (or an absolute/empty name) makes
// it dangerous.
//
// This isn't just a local hygiene check: name reaches here from
// vcsfs.RefWriter/RefReader (see refAdapter below), which a peer's 9P
// Twalk/Tcreate drives directly — the 9p server library performs no
// validation of its own on a wname/create-name element (confirmed
// against server/dispatch.go's tWalk: each element is passed straight to
// File.Walk with no rejection of ".." or embedded "/"), and vcsfs itself
// has no path logic for refs at all, passing name straight through to
// this package. A malicious peer with only PermWrite (not full local
// access) could otherwise point an arbitrary filesystem path outside
// .9vcs/refs — anywhere the serving process can write — at a ref value
// of their choosing, using a single wname/create-name string containing
// its own embedded "/../" sequences (not multiple small Twalk elements;
// see PLAN.md's writeup for why the multi-element form doesn't reach as
// far).
func validRefName(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") {
		return false
	}
	if path.Clean(name) != name {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

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
	return r.withRefLock(func() error {
		return atomicWriteFile(r.headFile(), []byte("ref: "+name+"\n"))
	})
}

func (r *repo) setHeadDetached(h patches.Hash) error {
	return r.withRefLock(func() error {
		return atomicWriteFile(r.headFile(), []byte(h.String()+"\n"))
	})
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
	if !validRefName(name) {
		return patches.Hash{}, false, fmt.Errorf("invalid ref name %q", name)
	}
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

// writeRefFileLocked atomically writes h as name's ref content. Callers
// must already hold this repo's ref lock (withRefLock) — this has no
// locking of its own, deliberately, so casWriteRef's whole
// compare-then-write sequence runs under a single lock acquisition, not
// two nested ones.
func (r *repo) writeRefFileLocked(name string, h patches.Hash) error {
	if err := os.MkdirAll(filepath.Dir(r.refPath(name)), 0o755); err != nil {
		return err
	}
	return atomicWriteFile(r.refPath(name), []byte(h.String()+"\n"))
}

// errRefConflict marks a CAS ref-write failure: the caller's view of the
// ref (old) no longer matches its actual current value. Wrapped, not
// returned bare, so a caller can errors.Is against it if it ever needs to
// distinguish this from other failures (a malformed request, an unknown
// hash) — reconcile currently just surfaces the message as-is, since base
// 9P2000 has no structured error codes to preserve the distinction across
// the wire anyway (see PLAN.md's library facts: no .u/.L extensions).
var errRefConflict = errors.New("ref changed since last observed")

// setRefHashCAS updates name's ref to new, but only if its current value
// is exactly old (the zero hash meaning "must not exist yet") — the
// write side of vcsfs's /refs contract (see vcsfs.RefWriter): a served
// peer connection pushing to this repo. Refuses to move the branch
// currently checked out here (see casWriteRef) — a rule specific to a
// network push, not to setLocalRefCAS's local callers.
func (r *repo) setRefHashCAS(name string, old, new patches.Hash) error {
	return r.casWriteRef(name, old, new, true)
}

// setLocalRefCAS is setRefHashCAS without the checked-out-branch
// refusal: every local mutating command (record, merge, checkout -b,
// branch, apply, reconcile/import's local pull) uses this in place of a
// blind, unconditional write, so a concurrent writer — another local
// command in a different terminal, or a live `serve`'s incoming push —
// produces a clean, reported conflict instead of silently discarding
// whichever write lost the race. Every call site already has the "old"
// hash it read earlier in scope (head, the branch's current tip, the
// caller's last-observed remote hash), so this is routing through the
// same compare-and-swap the network path always had, not new
// bookkeeping for callers.
func (r *repo) setLocalRefCAS(name string, old, new patches.Hash) error {
	return r.casWriteRef(name, old, new, false)
}

// casWriteRef is setRefHashCAS/setLocalRefCAS's shared implementation.
// refuseCheckedOutBranch is true only for the network-facing case — see
// its doc comment on setRefHashCAS's original version for the full
// rationale (a push moving the checked-out branch out from under the
// working tree without also updating it, which a local command never
// does, since it always updates both together in the same call).
//
// The whole compare-then-write sequence runs inside withRefLock — not
// just the final write — because that's what actually closes the race
// this exists for: checking "is old still current" and writing the new
// value have to be atomic together, or two callers can both pass the
// check before either writes.
func (r *repo) casWriteRef(name string, old, new patches.Hash, refuseCheckedOutBranch bool) error {
	if !new.IsZero() && !r.store.Has(new) {
		return fmt.Errorf("cannot point %q at unknown patch %s", name, new)
	}
	return r.withRefLock(func() error {
		if refuseCheckedOutBranch {
			if branch, err := r.currentBranch(); err != nil {
				return err
			} else if branch == name {
				return fmt.Errorf("refusing to update %q: it is the branch currently checked out here — the working tree would desync from it; check out a different branch here first, or push under a different name", name)
			}
		}

		current, exists, err := r.refHash(name)
		if err != nil {
			return err
		}
		if exists {
			if current != old {
				return fmt.Errorf("%w: %q is at %s, not %s (another 9vcs command updated it concurrently — re-run)", errRefConflict, name, current, old)
			}
		} else if !old.IsZero() {
			return fmt.Errorf("%w: %q does not exist, expected %s", errRefConflict, name, old)
		}
		return r.writeRefFileLocked(name, new)
	})
}

func (r *repo) mergeHeadFile() string { return filepath.Join(r.dir, "MERGE_HEAD") }

// mergeHeads reads the in-progress merge's other side(s), if any — one
// hash per line, the same MERGE_HEAD format git itself uses (which
// supports multiple lines for octopus merges, not a 9vcs invention).
// Their presence is what tells record to make the next patch depend on
// HEAD plus every merge head instead of just HEAD, and to finalize the
// merge rather than requiring changes. A nil/empty result means no merge
// is in progress.
func (r *repo) mergeHeads() ([]patches.Hash, error) {
	data, err := os.ReadFile(r.mergeHeadFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var heads []patches.Hash
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		h, err := patches.HashFromHex(line)
		if err != nil {
			return nil, err
		}
		heads = append(heads, h)
	}
	return heads, nil
}

// setMergeHeads writes heads, one per line, replacing whatever
// MERGE_HEAD held before. Atomic (temp file + rename) and lock-protected,
// same as every other ref/HEAD write (see atomicWriteFile/withRefLock) —
// a plain os.WriteFile here, unlike everywhere else, would let a crash
// mid-write leave a truncated MERGE_HEAD that HashFromHex then errors on
// for every subsequent command until manually removed, and would let a
// concurrent writer's bytes interleave with this one's.
func (r *repo) setMergeHeads(heads []patches.Hash) error {
	var b strings.Builder
	for _, h := range heads {
		b.WriteString(h.String())
		b.WriteString("\n")
	}
	return r.withRefLock(func() error {
		return atomicWriteFile(r.mergeHeadFile(), []byte(b.String()))
	})
}

func (r *repo) clearMergeHeads() error {
	return r.withRefLock(func() error {
		err := os.Remove(r.mergeHeadFile())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

func (r *repo) mergeSidecarsFile() string { return filepath.Join(r.dir, "MERGE_SIDECARS") }

// binaryConflictSidecar is the path merge/apply writes a losing side's
// content to, alongside a binary conflict — e.g. "logo.png.a1b2c3d4e5f6"
// next to "logo.png", which keeps roots[0]'s ("ours") content. Named by
// short hash rather than a fixed ".theirs" suffix so apply's N-way case
// can write one sidecar per differing side without a naming collision —
// merge's own two-way case just calls this once, with its one "theirs"
// hash. It's a comparison aid, not tracked content: record deletes it
// once the merge is finalized (see mergeSidecars/setMergeSidecars).
func binaryConflictSidecar(path string, side patches.Hash) string {
	return path + "." + side.String()[:12]
}

// writeSidecarFile writes a binary-conflict comparison sidecar's content,
// confined to r.root via os.Root — the same real, live-proven bug class
// writeWorkingTree was fixed for (see its doc comment): sidecar's path
// string is a legitimate join of an already-validated tracked path plus a
// hash suffix, but a plain filepath.Join+os.WriteFile still follows
// whatever's *already on disk* at an intermediate path component. A
// symlink there — planted by an earlier, unrelated, already-recorded
// commit, or simply pre-existing in the victim's working tree (e.g. a
// symlinked vendor/ or build-cache dir) — sends this write straight
// outside the repo. os.Root refuses that the same way it does for
// writeWorkingTree.
func writeSidecarFile(r *repo, sidecar string, data []byte) error {
	root, err := os.OpenRoot(r.root)
	if err != nil {
		return err
	}
	defer root.Close()
	rel := filepath.FromSlash(sidecar)
	if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	return root.WriteFile(rel, data, 0o644)
}

// removeSidecarFile removes a sidecar written by writeSidecarFile, same
// os.Root confinement as the write side. Mirrors plain os.Remove's
// ErrNotExist-is-fine contract that callers already relied on.
func removeSidecarFile(r *repo, sidecar string) error {
	root, err := os.OpenRoot(r.root)
	if err != nil {
		return err
	}
	defer root.Close()
	return root.Remove(filepath.FromSlash(sidecar))
}

// setMergeSidecars records every sidecar path merge wrote, so record knows
// what to clean up once it finalizes — these are merge tooling, not
// content the user asked to track. Atomic and lock-protected, same
// reasoning as setMergeHeads.
func (r *repo) setMergeSidecars(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	return r.withRefLock(func() error {
		return atomicWriteFile(r.mergeSidecarsFile(), []byte(strings.Join(paths, "\n")+"\n"))
	})
}

func (r *repo) mergeSidecars() ([]string, error) {
	data, err := os.ReadFile(r.mergeSidecarsFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func (r *repo) clearMergeSidecars() error {
	return r.withRefLock(func() error {
		err := os.Remove(r.mergeSidecarsFile())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

func (r *repo) authorizedPeersFile() string { return filepath.Join(r.dir, "authorized-peers") }

// refAdapter exposes repo's ref storage as a vcsfs.RefReader without
// vcsfs needing to import cmd/9vcs (that would be backwards) and without
// repo's own ref methods needing to be exported just for this — Go
// interfaces are satisfied structurally, so this small same-package
// wrapper is enough.
type refAdapter struct{ r *repo }

func (a refAdapter) RefHash(name string) (patches.Hash, bool, error) { return a.r.refHash(name) }
func (a refAdapter) ListRefs() ([]string, error)                     { return a.r.listBranches() }
func (a refAdapter) SetRefHash(name string, old, new patches.Hash) error {
	return a.r.setRefHashCAS(name, old, new)
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
// every regular file or symlink outside .9vcs. WalkDir doesn't follow a
// symlink to see what it points at (a symlink-to-directory is reported
// as a plain leaf entry, never descended into), so no special handling
// is needed there — this just has to stop excluding symlink entries
// outright the way it used to.
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
		if !d.Type().IsRegular() && d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out, err
}
