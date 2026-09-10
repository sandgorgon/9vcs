// Package repo is the repo-state / working-tree-diff orchestration
// underlying the `9vcs` CLI (cmd/9vcs), extracted into an importable
// package — GitHub issue #26 — so an external Go program can open a
// repo, resolve a ref to a patches.Index, and compute a working-tree
// diff without shelling out to the 9vcs binary and parsing its text
// output. cmd/9vcs is now a thin consumer of this package; no CLI
// behavior changed as part of the move.
package repo

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sandgorgon/9vcs/fsx"
	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/synth"
)

// DotDir is the on-disk root for all local repo state, sibling to .git.
const DotDir = ".9vcs"

// DefaultBranch is the branch `init` points HEAD at.
const DefaultBranch = "main"

// Repo resolves the paths and stores for one 9vcs repository.
type Repo struct {
	FS     fsx.FS   // ref/HEAD/lock and patch/blob storage — see PLAN.md decision #9
	Tree   fsx.Tree // working-tree materialization — nil if unavailable, see ErrWorkingTreeUnsupported
	Root   string   // working tree root (parent of .9vcs), in FS's own path convention
	Dir    string   // .9vcs, in FS's own path convention
	Store  *patches.Store
	Blobs  *patches.BlobStore
	Offers *patches.BlobStore // pending offer bundles received via `9vcs serve`'s /offers — see PLAN.md decision #8
	cache  *synth.Cache       // memoizes Materialize within this one invocation
}

var ErrNotARepo = errors.New("not a 9vcs repository (or any parent directory)")

// ErrWorkingTreeUnsupported marks a working-tree-materialization call
// (WriteSidecarFile/RemoveSidecarFile/WorkingFiles) refusing because
// r.Tree is nil — reached only via OpenFS called directly with a
// non-local FS and no Tree (FindAt, the normal namespace-resolved
// entry point, always builds a real one — see openFSAt).
var ErrWorkingTreeUnsupported = errors.New("repo: working-tree operations aren't available for this repo — see PLAN.md decision #9")

// Find walks up from the current directory looking for .9vcs, the same
// way git walks up looking for .git. Always resolves against the real
// OS filesystem — see PLAN.md decision #9: the implicit, no-argument
// case is deliberately untouched by namespace-first resolution; use
// FindAt (reached via the CLI's -C flag) for that.
func Find() (*Repo, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	dir := cwd
	for {
		candidate := filepath.Join(dir, DotDir)
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return Open(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, ErrNotARepo
		}
		dir = parent
	}
}

// Open returns the Repo rooted at root on the real OS filesystem.
func Open(root string) (*Repo, error) {
	return OpenFS(fsx.NewOS(""), root)
}

// OpenFS returns the Repo rooted at root within fs — see PLAN.md
// decision #9. Tree (working-tree materialization) is built too when
// fs is local (osfs); a non-local fs opened directly through this
// function (rather than FindAt, which has the extra connection state
// needed to build a real p9Tree — see openFSAt) gets a nil Tree, and
// working-tree operations return ErrWorkingTreeUnsupported.
func OpenFS(fs fsx.FS, root string) (*Repo, error) {
	dir := fs.Join(root, DotDir)
	store, err := patches.OpenFS(fs, fs.Join(dir, "patches"))
	if err != nil {
		return nil, err
	}
	blobs, err := patches.OpenBlobsFS(fs, fs.Join(dir, "blobs"))
	if err != nil {
		return nil, err
	}
	offers, err := patches.OpenBlobsFS(fs, fs.Join(dir, "offers"))
	if err != nil {
		return nil, err
	}
	r := &Repo{FS: fs, Root: root, Dir: dir, Store: store, Blobs: blobs, Offers: offers, cache: synth.NewCache(store)}
	if fs.IsLocal() {
		tree, err := fsx.NewOSTree(root)
		if err != nil {
			return nil, err
		}
		r.Tree = tree
	}
	return r, nil
}

// openFSAt is OpenFS plus building a real Tree from nc when the root
// resolved through the namespace (nc != nil) — FindAt's entry point,
// separate from the public OpenFS because only namespace resolution
// has the underlying *client.Client/root Fid a p9Tree needs.
func openFSAt(fs fsx.FS, nc *namespaceConn, root string) (*Repo, error) {
	r, err := OpenFS(fs, root)
	if err != nil {
		return nil, err
	}
	if nc != nil {
		r.Tree = fsx.NewP9Tree(nc.c, nc.root, root)
	}
	return r, nil
}

// Materialize is patches.Materialize(r.Store, roots...), memoized for
// the lifetime of this Repo value (i.e. this one command invocation) —
// see synth.Cache. A single command commonly replays overlapping
// closures more than once (a merge preview materializes ours, theirs,
// and their union all in one call), so every command in this package
// should call this instead of patches.Materialize directly.
func (r *Repo) Materialize(roots ...patches.Hash) (patches.Index, error) {
	return r.cache.Materialize(roots...)
}

// refLockPath is the cross-process advisory lock every ref/HEAD mutation
// takes for its critical section — see withRefLock.
func (r *Repo) refLockPath() string { return r.FS.Join(r.Dir, "lock") }

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
// — the actual mutual exclusion setRefHashCAS/SetLocalRefCAS/
// SetHeadBranch/SetHeadDetached need. An in-memory mutex (what this
// repo used before) only ever guards goroutines within one process; two
// separate local CLI invocations, or a local command racing a live
// `serve`'s incoming push, are different OS processes with no shared
// memory to synchronize through at all. Go's stdlib has no flock
// primitive, and this project stays stdlib-only (see PLAN.md), so this
// uses fsx.FS.Lock as the actual mutex primitive instead: atomically
// creating the lock file is the acquire, removing it is the release —
// same shape as many tools' simple lockfile convention. Works
// identically over a p9fs-backed Repo since github.com/sandgorgon/9p
// v0.8.0 (see PLAN.md decision #9): Lock is true exclusive-create on
// either backend.
func (r *Repo) withRefLock(fn func() error) error {
	path := r.refLockPath()
	deadline := time.Now().Add(refLockAcquireTimeout)
	for {
		err := r.FS.Lock(path)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("acquiring ref lock: %w", err)
		}
		if info, statErr := r.FS.Stat(path); statErr == nil && info.Exists && time.Since(info.ModTime) > refLockStaleAge {
			r.FS.Remove(path) // best-effort: if another stealer wins this race, the next loop iteration's Lock sorts it out
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ref lock %s is held by another 9vcs process; if you're sure nothing else is running against this repo, remove it manually", path)
		}
		time.Sleep(refLockRetryInterval)
	}
	defer r.FS.Remove(path)
	return fn()
}

func (r *Repo) headFile() string { return r.FS.Join(r.Dir, "HEAD") }

func (r *Repo) refPath(name string) string { return r.FS.Join(r.Dir, "refs", name) }

// ValidRefName mirrors objstore/patches' FileChange.Path validation —
// same shape, same reason: refPath joins name straight onto r.Dir via
// fs.Join, and nested branch names are a real, intentional feature
// (writeRefFileLocked's MkdirAll), so name can't just be rejected for
// containing "/" — only a ".." segment (or an absolute/empty name) makes
// it dangerous.
//
// This isn't just a local hygiene check: name reaches here from a peer's
// 9P Twalk/Tcreate driving *Repo directly (see vcsfs.RefReader/
// RefWriter, satisfied structurally by Repo's RefHash/ListRefs/
// SetRefHash) — the 9p server library performs no validation of its own
// on a wname/create-name element either (confirmed against
// server/dispatch.go's tWalk: each Wname element is passed straight to
// File.Walk, no rejection of ".." or embedded "/"), and vcsfs itself has
// no path logic for refs at all, passing name straight through to this
// package. A malicious peer with only PermWrite (not full local access)
// could otherwise point an arbitrary filesystem path outside
// .9vcs/refs — anywhere the serving process can write — at a ref value
// of their choosing, using a single wname/create-name string containing
// its own embedded "/../" sequences (not multiple small Twalk elements;
// see PLAN.md's writeup for why the multi-element form doesn't reach as
// far).
func ValidRefName(name string) bool {
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

// readFileOrAbsent reads path via r.FS, returning (nil, false, nil) if
// it doesn't exist and (nil, false, err) for any other failure.
// Existence is checked via Stat rather than matching ReadFile's error
// against os.ErrNotExist directly, because base 9P2000 has no
// structured error code to do that reliably over a p9fs-backed repo
// (see PLAN.md's library facts) — fsx.FS.Stat already handles this
// correctly for either backend.
func (r *Repo) readFileOrAbsent(path string) ([]byte, bool, error) {
	data, err := r.FS.ReadFile(path)
	if err == nil {
		return data, true, nil
	}
	if info, statErr := r.FS.Stat(path); statErr == nil && !info.Exists {
		return nil, false, nil
	}
	return nil, false, err
}

// CurrentBranch returns the branch name HEAD points to, or "" if HEAD is
// detached (points directly at a patch hash instead of a branch name).
func (r *Repo) CurrentBranch() (string, error) {
	data, err := r.FS.ReadFile(r.headFile())
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(data))
	if branch, ok := strings.CutPrefix(s, "ref: "); ok {
		return branch, nil
	}
	return "", nil
}

func (r *Repo) SetHeadBranch(name string) error {
	return r.withRefLock(func() error {
		return r.FS.WriteAtomic(r.headFile(), []byte("ref: "+name+"\n"))
	})
}

func (r *Repo) SetHeadDetached(h patches.Hash) error {
	return r.withRefLock(func() error {
		return r.FS.WriteAtomic(r.headFile(), []byte(h.String()+"\n"))
	})
}

// HeadHash resolves HEAD (symbolic or detached) to a concrete patch hash.
// ok is false only when HEAD is a branch that has no patches recorded yet.
func (r *Repo) HeadHash() (patches.Hash, bool, error) {
	branch, err := r.CurrentBranch()
	if err != nil {
		return patches.Hash{}, false, err
	}
	if branch == "" {
		data, err := r.FS.ReadFile(r.headFile())
		if err != nil {
			return patches.Hash{}, false, err
		}
		h, err := patches.HashFromHex(strings.TrimSpace(string(data)))
		return h, true, err
	}
	return r.RefHash(branch)
}

// RefHash reads the head patch hash of branch name, if it has one yet.
func (r *Repo) RefHash(name string) (patches.Hash, bool, error) {
	if !ValidRefName(name) {
		return patches.Hash{}, false, fmt.Errorf("invalid ref name %q", name)
	}
	data, ok, err := r.readFileOrAbsent(r.refPath(name))
	if err != nil || !ok {
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
func (r *Repo) writeRefFileLocked(name string, h patches.Hash) error {
	if err := r.FS.MkdirAll(r.FS.Dir(r.refPath(name))); err != nil {
		return err
	}
	return r.FS.WriteAtomic(r.refPath(name), []byte(h.String()+"\n"))
}

// ErrRefConflict marks a CAS ref-write failure: the caller's view of the
// ref (old) no longer matches its actual current value. Wrapped, not
// returned bare, so a caller can errors.Is against it if it ever needs to
// distinguish this from other failures (a malformed request, an unknown
// hash) — reconcile currently just surfaces the message as-is, since base
// 9P2000 has no structured error codes to preserve the distinction across
// the wire anyway (see PLAN.md's library facts: no .u/.L extensions).
var ErrRefConflict = errors.New("ref changed since last observed")

// SetRefHash updates name's ref to new, but only if its current value is
// exactly old (the zero hash meaning "must not exist yet") — the write
// side of vcsfs's /refs contract (see vcsfs.RefWriter, which *Repo
// satisfies directly by this method's exact name/signature): a served
// peer connection pushing to this repo. Refuses to move the branch
// currently checked out here (see casWriteRef) — a rule specific to a
// network push, not to SetLocalRefCAS's local callers.
func (r *Repo) SetRefHash(name string, old, new patches.Hash) error {
	return r.casWriteRef(name, old, new, true)
}

// SetLocalRefCAS is SetRefHash without the checked-out-branch refusal:
// every local mutating command (record, merge, checkout -b, branch,
// apply, reconcile/import's local pull) uses this in place of a blind,
// unconditional write, so a concurrent writer — another local command in
// a different terminal, or a live `serve`'s incoming push — produces a
// clean, reported conflict instead of silently discarding whichever
// write lost the race. Every call site already has the "old" hash it
// read earlier in scope (head, the branch's current tip, the caller's
// last-observed remote hash), so this is routing through the same
// compare-and-swap the network path always had, not new bookkeeping for
// callers.
func (r *Repo) SetLocalRefCAS(name string, old, new patches.Hash) error {
	return r.casWriteRef(name, old, new, false)
}

// casWriteRef is SetRefHash/SetLocalRefCAS's shared implementation.
// refuseCheckedOutBranch is true only for the network-facing case — see
// its doc comment on SetRefHash's original version for the full
// rationale (a push moving the checked-out branch out from under the
// working tree without also updating it, which a local command never
// does, since it always updates both together in the same call).
//
// The whole compare-then-write sequence runs inside withRefLock — not
// just the final write — because that's what actually closes the race
// this exists for: checking "is old still current" and writing the new
// value have to be atomic together, or two callers can both pass the
// check before either writes.
func (r *Repo) casWriteRef(name string, old, new patches.Hash, refuseCheckedOutBranch bool) error {
	if !new.IsZero() && !r.Store.Has(new) {
		return fmt.Errorf("cannot point %q at unknown patch %s", name, new)
	}
	return r.withRefLock(func() error {
		if refuseCheckedOutBranch {
			if branch, err := r.CurrentBranch(); err != nil {
				return err
			} else if branch == name {
				return fmt.Errorf("refusing to update %q: it is the branch currently checked out here — the working tree would desync from it; check out a different branch here first, or push under a different name", name)
			}
		}

		current, exists, err := r.RefHash(name)
		if err != nil {
			return err
		}
		if exists {
			if current != old {
				return fmt.Errorf("%w: %q is at %s, not %s (another 9vcs command updated it concurrently — re-run)", ErrRefConflict, name, current, old)
			}
		} else if !old.IsZero() {
			return fmt.Errorf("%w: %q does not exist, expected %s", ErrRefConflict, name, old)
		}
		return r.writeRefFileLocked(name, new)
	})
}

func (r *Repo) mergeHeadFile() string { return r.FS.Join(r.Dir, "MERGE_HEAD") }

// MergeHeads reads the in-progress merge's other side(s), if any — one
// hash per line, the same MERGE_HEAD format git itself uses (which
// supports multiple lines for octopus merges, not a 9vcs invention).
// Their presence is what tells record to make the next patch depend on
// HEAD plus every merge head instead of just HEAD, and to finalize the
// merge rather than requiring changes. A nil/empty result means no merge
// is in progress.
func (r *Repo) MergeHeads() ([]patches.Hash, error) {
	data, ok, err := r.readFileOrAbsent(r.mergeHeadFile())
	if err != nil || !ok {
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

// SetMergeHeads writes heads, one per line, replacing whatever
// MERGE_HEAD held before. Atomic (temp file + rename) and lock-protected,
// same as every other ref/HEAD write (see WriteAtomic/withRefLock) —
// a plain write here, unlike everywhere else, would let a crash
// mid-write leave a truncated MERGE_HEAD that HashFromHex then errors on
// for every subsequent command until manually removed, and would let a
// concurrent writer's bytes interleave with this one's.
func (r *Repo) SetMergeHeads(heads []patches.Hash) error {
	var b strings.Builder
	for _, h := range heads {
		b.WriteString(h.String())
		b.WriteString("\n")
	}
	return r.withRefLock(func() error {
		return r.FS.WriteAtomic(r.mergeHeadFile(), []byte(b.String()))
	})
}

func (r *Repo) ClearMergeHeads() error {
	return r.withRefLock(func() error {
		return r.FS.Remove(r.mergeHeadFile())
	})
}

func (r *Repo) mergeSidecarsFile() string { return r.FS.Join(r.Dir, "MERGE_SIDECARS") }

// BinaryConflictSidecar is the path merge/apply writes a losing side's
// content to, alongside a binary conflict — e.g. "logo.png.a1b2c3d4e5f6"
// next to "logo.png", which keeps roots[0]'s ("ours") content. Named by
// short hash rather than a fixed ".theirs" suffix so apply's N-way case
// can write one sidecar per differing side without a naming collision —
// merge's own two-way case just calls this once, with its one "theirs"
// hash. It's a comparison aid, not tracked content: record deletes it
// once the merge is finalized (see MergeSidecars/SetMergeSidecars).
func BinaryConflictSidecar(path string, side patches.Hash) string {
	return path + "." + side.String()[:12]
}

// WriteSidecarFile writes a binary-conflict comparison sidecar's
// content, through r.Tree — confined to the working tree root the
// same way every other Tree operation is (see fsx.Tree's doc
// comment): sidecar's path string is a legitimate join of an
// already-validated tracked path plus a hash suffix, but a naive
// write would still follow whatever's *already on disk* at an
// intermediate path component. A symlink there — planted by an
// earlier, unrelated, already-recorded commit, or simply pre-existing
// in the victim's working tree (e.g. a symlinked vendor/ or
// build-cache dir) — would otherwise send this write straight outside
// the repo; this is the exact live bug class WriteWorkingTree's own
// doc comment describes.
func WriteSidecarFile(r *Repo, sidecar string, data []byte) error {
	if r.Tree == nil {
		return ErrWorkingTreeUnsupported
	}
	return r.Tree.WriteFile(sidecar, data, false)
}

// RemoveSidecarFile removes a sidecar written by WriteSidecarFile,
// same confinement. Idempotent, like every other Tree.Remove call.
func RemoveSidecarFile(r *Repo, sidecar string) error {
	if r.Tree == nil {
		return ErrWorkingTreeUnsupported
	}
	return r.Tree.Remove(sidecar)
}

// SetMergeSidecars records every sidecar path merge wrote, so record knows
// what to clean up once it finalizes — these are merge tooling, not
// content the user asked to track. Atomic and lock-protected, same
// reasoning as SetMergeHeads.
func (r *Repo) SetMergeSidecars(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	return r.withRefLock(func() error {
		return r.FS.WriteAtomic(r.mergeSidecarsFile(), []byte(strings.Join(paths, "\n")+"\n"))
	})
}

func (r *Repo) MergeSidecars() ([]string, error) {
	data, ok, err := r.readFileOrAbsent(r.mergeSidecarsFile())
	if err != nil || !ok {
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

func (r *Repo) ClearMergeSidecars() error {
	return r.withRefLock(func() error {
		return r.FS.Remove(r.mergeSidecarsFile())
	})
}

func (r *Repo) AuthorizedPeersFile() string { return r.FS.Join(r.Dir, "authorized-peers") }

// ListRefs returns every branch name with a ref file, sorted. Named to
// match vcsfs.RefReader's method exactly, alongside RefHash/SetRefHash,
// so *Repo satisfies vcsfs.RefReader/RefWriter directly — vcsfs can't
// import this package (that would be backwards), but Go interfaces are
// satisfied structurally, so no adapter type is needed.
func (r *Repo) ListRefs() ([]string, error) {
	names, err := r.FS.ReadDir(r.FS.Join(r.Dir, "refs"))
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// ResolveRef resolves arg to a patch hash: an exact branch name first, then
// a full or abbreviated patch hash.
func (r *Repo) ResolveRef(arg string) (patches.Hash, error) {
	if h, ok, err := r.RefHash(arg); err != nil {
		return patches.Hash{}, err
	} else if ok {
		return h, nil
	}
	h, err := r.Store.ResolvePrefix(arg)
	if err != nil {
		return patches.Hash{}, err
	}
	return h, nil
}

// WorkingFiles walks the working tree, returning repo-relative paths for
// every regular file or symlink outside .9vcs, via r.Tree — backend-
// agnostic (osTree or p9Tree, see fsx.Tree). A symlink is never
// descended into, tracked-directory-or-not: TreeInfo.IsDir and
// IsSymlink are mutually exclusive by construction on both backends
// (Lstat-based, not Stat-based), so a symlink is always reported as a
// leaf entry, matching how git/every other caller here treats one.
func (r *Repo) WorkingFiles() ([]string, error) {
	if r.Tree == nil {
		return nil, ErrWorkingTreeUnsupported
	}
	var out []string
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := r.Tree.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Name == DotDir && e.Info.IsDir {
				continue
			}
			rel := e.Name
			if dir != "" {
				rel = dir + "/" + e.Name
			}
			if e.Info.IsDir {
				if err := walk(rel); err != nil {
					return err
				}
				continue
			}
			out = append(out, rel)
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return out, nil
}
