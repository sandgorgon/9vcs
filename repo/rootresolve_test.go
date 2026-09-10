package repo

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sandgorgon/9p/examples/dirfs"
	"github.com/sandgorgon/9p/server"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// serveNamespace serves tmproot over a Unix socket via dirfs — the same
// server github.com/sandgorgon/9sh's real /local binding uses in
// production (see PLAN.md decision #9) — and points $_9SH_UNIX_SOCK at
// it for the duration of the test.
func serveNamespace(t *testing.T, tmproot string) {
	t.Helper()
	fs, err := dirfs.New(tmproot)
	if err != nil {
		t.Fatal(err)
	}
	// A short path under the OS temp dir, not t.TempDir()'s (often
	// deeply nested) directory — AF_UNIX socket paths have a low length
	// limit on most platforms.
	sockDir, err := os.MkdirTemp("", "9vcs-p9-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "s.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &server.Server{FS: fs}
	go srv.Serve(ln)

	t.Setenv("_9SH_UNIX_SOCK", sockPath)
}

func TestFindAtResolvesThroughNamespace(t *testing.T) {
	tmproot := t.TempDir()
	repoDir := filepath.Join(tmproot, "local", "myrepo")
	if err := os.MkdirAll(filepath.Join(repoDir, DotDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	local, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SetHeadBranch(DefaultBranch); err != nil {
		t.Fatal(err)
	}
	patchHash, err := local.Store.Put(&patches.Patch{Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SetLocalRefCAS(DefaultBranch, patches.Hash{}, patchHash); err != nil {
		t.Fatal(err)
	}

	serveNamespace(t, tmproot)

	r, err := FindAt("myrepo")
	if err != nil {
		t.Fatal(err)
	}
	if r.FS.IsLocal() {
		t.Fatal("expected a p9fs-backed Repo, got a local one — namespace resolution didn't take")
	}

	// Reads work.
	h, ok, err := r.RefHash(DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || h != patchHash {
		t.Errorf("RefHash(%q) = %s, %v, want %s, true", DefaultBranch, h, ok, patchHash)
	}
	p, err := r.Store.Get(patchHash)
	if err != nil {
		t.Fatal(err)
	}
	if p.Message != "hello" {
		t.Errorf("got message %q", p.Message)
	}
	branches, err := r.ListRefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0] != DefaultBranch {
		t.Errorf("ListRefs() = %v", branches)
	}

	// Ref writes work over p9fs as of github.com/sandgorgon/9p v0.8.0
	// (Rename/Remove landed — see PLAN.md decision #9's Gap 1). The
	// same withRefLock/casWriteRef logic osfs uses runs unmodified here.
	newHash, err := r.Store.Put(&patches.Patch{Dependencies: []patches.Hash{patchHash}, Message: "second"})
	if err != nil {
		t.Fatalf("Store.Put over p9fs: %v", err)
	}
	if err := r.SetLocalRefCAS(DefaultBranch, patchHash, newHash); err != nil {
		t.Fatalf("SetLocalRefCAS over p9fs: %v", err)
	}
	h2, ok2, err := r.RefHash(DefaultBranch)
	if err != nil || !ok2 || h2 != newHash {
		t.Errorf("ref after write: RefHash = %s, %v, %v, want %s, true, nil", h2, ok2, err, newHash)
	}
	// And the write is genuinely visible back through the original,
	// osfs-backed Repo too — same real files underneath either backend.
	if h3, ok3, err := local.RefHash(DefaultBranch); err != nil || !ok3 || h3 != newHash {
		t.Errorf("local.RefHash after p9fs write = %s, %v, %v, want %s, true, nil", h3, ok3, err, newHash)
	}

	// A stale CAS write still correctly conflicts, over p9fs exactly as
	// it does locally.
	if err := r.SetLocalRefCAS(DefaultBranch, patchHash, newHash); !errors.Is(err, ErrRefConflict) {
		t.Errorf("stale CAS write over p9fs: got %v, want ErrRefConflict", err)
	}

	// Working-tree materialization (Phase 3, github.com/sandgorgon/9p
	// v0.9.0's 9P2000.u symlink support) works fully over p9fs too:
	// checkout's real end-to-end flow — read the current tree via
	// ChangedFiles, write a new one via WriteWorkingTree — including a
	// symlink and an executable bit, verified against the real files
	// dirfs is serving underneath. Uses KindBlob rather than KindText
	// for the plain-content case: FileGraph construction is package-
	// internal, and text-diffing specifics aren't what this test is
	// about — the p9fs plumbing and symlink handling are.
	if r.Tree == nil {
		t.Fatal("expected a non-nil Tree for a p9fs-backed Repo")
	}
	mainGoHash, err := r.Blobs.Put([]byte("package main\n"))
	if err != nil {
		t.Fatal(err)
	}
	target := patches.Index{
		"src/main.go": patches.PathState{Kind: patches.KindBlob, Blob: mainGoHash},
		"bin/run.sh":  patches.PathState{Kind: patches.KindBlob, Blob: mainGoHash, Executable: true},
		"bin/env":     patches.PathState{Kind: patches.KindSymlink, SymlinkTarget: "/usr/bin/env"},
	}
	if err := WriteWorkingTree(r, patches.Index{}, target); err != nil {
		t.Fatalf("WriteWorkingTree over p9fs: %v", err)
	}

	// Verify against the real underlying files (dirfs is just serving
	// tmproot/local/myrepo directly).
	got, err := os.ReadFile(filepath.Join(repoDir, "src", "main.go"))
	if err != nil || string(got) != "package main\n" {
		t.Errorf("src/main.go = %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(repoDir, "bin", "run.sh")); err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("bin/run.sh not executable: %v, %v", info, err)
	}
	if target2, err := os.Readlink(filepath.Join(repoDir, "bin", "env")); err != nil || target2 != "/usr/bin/env" {
		t.Errorf("bin/env symlink = %q, %v, want /usr/bin/env", target2, err)
	}

	// And the read side (ChangedFiles) correctly reports that same tree
	// back — including recognizing the symlink and executable bit
	// rather than reading through them (see fsx.Tree's doc comment on
	// why this needs Lstat semantics, not Stat).
	changed, err := ChangedFiles(r, patches.Index{})
	if err != nil {
		t.Fatalf("ChangedFiles over p9fs: %v", err)
	}
	if fc, ok := changed["bin/env"]; !ok || fc.Kind != patches.KindSymlink || fc.SymlinkTarget != "/usr/bin/env" {
		t.Errorf("ChangedFiles[bin/env] = %+v, %v", fc, ok)
	}
	if fc, ok := changed["bin/run.sh"]; !ok || !fc.Executable {
		t.Errorf("ChangedFiles[bin/run.sh] = %+v, %v, want Executable=true", fc, ok)
	}
	if _, ok := changed["src/main.go"]; !ok {
		t.Error("ChangedFiles missing src/main.go")
	}
}

// TestConcurrentSetLocalRefCASOnlyOneWinsOverP9FS is
// reflock_test.go's TestConcurrentSetLocalRefCASOnlyOneWins, over a
// namespace-resolved (p9fs) Repo instead of a local one — the actual
// property this whole feature exists to preserve: withRefLock's mutual
// exclusion has to hold exactly as well over a 9P connection as it
// does over direct syscalls, not just "eventually converge."
func TestConcurrentSetLocalRefCASOnlyOneWinsOverP9FS(t *testing.T) {
	tmproot := t.TempDir()
	repoDir := filepath.Join(tmproot, "local", "myrepo")
	if err := os.MkdirAll(filepath.Join(repoDir, DotDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	local, err := Open(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SetHeadBranch(DefaultBranch); err != nil {
		t.Fatal(err)
	}
	base, err := local.Store.Put(&patches.Patch{Message: "base"})
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SetLocalRefCAS("contended", patches.Hash{}, base); err != nil {
		t.Fatal(err)
	}

	serveNamespace(t, tmproot)

	const n = 10
	candidates := make([]patches.Hash, n)
	for i := range n {
		h, err := local.Store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "candidate"})
		if err != nil {
			t.Fatal(err)
		}
		candidates[i] = h
	}

	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine resolves its own Repo (its own dial/Attach,
			// its own *client.Client) — this is standing in for separate
			// process invocations, same as the local version of this test.
			r, err := FindAt("myrepo")
			if err != nil {
				results[i] = err
				return
			}
			results[i] = r.SetLocalRefCAS("contended", base, candidates[i])
		}(i)
	}
	wg.Wait()

	successes := 0
	var winner patches.Hash
	for i, err := range results {
		if err == nil {
			successes++
			winner = candidates[i]
		} else if !errors.Is(err, ErrRefConflict) {
			t.Errorf("candidate %d failed with a non-conflict error: %v", i, err)
		}
	}
	if successes != 1 {
		t.Fatalf("%d of %d concurrent writers succeeded, want exactly 1", successes, n)
	}
	if got, _, _ := local.RefHash("contended"); got != winner {
		t.Errorf("final ref = %s, want the one writer that actually succeeded (%s)", got, winner)
	}
}

func TestFindIgnoresNamespaceEvenWhenSet(t *testing.T) {
	tmproot := t.TempDir()
	repoDir := filepath.Join(tmproot, "local", "myrepo")
	if err := os.MkdirAll(filepath.Join(repoDir, DotDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(repoDir); err != nil {
		t.Fatal(err)
	}
	serveNamespace(t, tmproot)

	// A real, empty directory with no .9vcs anywhere above it — Find()
	// (no -C) must never consult the namespace to "find" the repo living
	// at local/myrepo within it, even though $_9SH_UNIX_SOCK is set.
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	if _, err := Find(); !errors.Is(err, ErrNotARepo) {
		t.Errorf("Find() with no -C: got %v, want ErrNotARepo — it must not have used the namespace", err)
	}
}
