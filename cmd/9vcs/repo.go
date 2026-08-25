package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

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

	// refMu guards setRefHashCAS's check-then-write against concurrent
	// peer connections within one `9vcs serve` process — the only case
	// where ref writes are actually concurrent (every local command is
	// its own process invocation, not a goroutine racing others in the
	// same one). Cross-process races on the plain ref files remain
	// unguarded, same as every other local write in this repo; not
	// something reconcile introduces or is trying to solve.
	refMu sync.Mutex
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
// write side of vcsfs's /refs contract (see vcsfs.RefWriter), and the
// only ref-write path that needs to guard against a concurrent write
// from another peer connection; see refMu.
func (r *repo) setRefHashCAS(name string, old, new patches.Hash) error {
	if !new.IsZero() && !r.store.Has(new) {
		return fmt.Errorf("cannot point %q at unknown patch %s", name, new)
	}
	r.refMu.Lock()
	defer r.refMu.Unlock()

	// Refuse to move the branch currently checked out here — same
	// default git ships (receive.denyCurrentBranch=refuse). Updating it
	// out from under the working tree wouldn't corrupt anything (the
	// object store stays correct either way), but the working tree would
	// silently stop matching HEAD's ref until someone happens to
	// checkout/diff and gets a confusing wall of "uncommitted changes"
	// that were never actually made locally. A local `record` or `merge`
	// never hits this: they always update the working tree and the ref
	// together, in the same command.
	if branch, err := r.currentBranch(); err != nil {
		return err
	} else if branch == name {
		return fmt.Errorf("refusing to update %q: it is the branch currently checked out here — the working tree would desync from it; check out a different branch here first, or push under a different name", name)
	}

	current, exists, err := r.refHash(name)
	if err != nil {
		return err
	}
	if exists {
		if current != old {
			return fmt.Errorf("%w: %q is at %s, not %s", errRefConflict, name, current, old)
		}
	} else if !old.IsZero() {
		return fmt.Errorf("%w: %q does not exist, expected %s", errRefConflict, name, old)
	}
	return r.setRefHash(name, new)
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
// MERGE_HEAD held before.
func (r *repo) setMergeHeads(heads []patches.Hash) error {
	var b strings.Builder
	for _, h := range heads {
		b.WriteString(h.String())
		b.WriteString("\n")
	}
	return os.WriteFile(r.mergeHeadFile(), []byte(b.String()), 0o644)
}

func (r *repo) clearMergeHeads() error {
	err := os.Remove(r.mergeHeadFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
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

// setMergeSidecars records every sidecar path merge wrote, so record knows
// what to clean up once it finalizes — these are merge tooling, not
// content the user asked to track.
func (r *repo) setMergeSidecars(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	return os.WriteFile(r.mergeSidecarsFile(), []byte(strings.Join(paths, "\n")+"\n"), 0o644)
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
	err := os.Remove(r.mergeSidecarsFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
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
