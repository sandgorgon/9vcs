package fsx

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/sandgorgon/9p/client"
	"github.com/sandgorgon/9p/examples/dirfs"
	"github.com/sandgorgon/9p/server"
)

// dialTreeUnix serves root over a real Unix-socket 9P connection
// negotiated with 9P2000.u — the same option repo.ResolveRoot uses —
// backed by dirfs, and returns the client plus its attached root Fid.
func dialTreeUnix(t *testing.T, root string) (*client.Client, *client.Fid) {
	t.Helper()
	fs, err := dirfs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	sockDir, err := os.MkdirTemp("", "9vcs-tree-test-*")
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

	c, err := client.Dial("unix", sockPath, client.WithUnixExtensions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	root9, err := c.Attach("test", "")
	if err != nil {
		t.Fatal(err)
	}
	return c, root9
}

// treeBackends returns both Tree implementations rooted at a fresh
// temp dir each, so every test below runs against osTree and p9Tree
// identically.
func treeBackends(t *testing.T) map[string]Tree {
	t.Helper()
	osRoot := t.TempDir()
	osT, err := NewOSTree(osRoot)
	if err != nil {
		t.Fatal(err)
	}

	p9Root := t.TempDir()
	c, root9 := dialTreeUnix(t, p9Root)
	p9T := NewP9Tree(c, root9, "")

	return map[string]Tree{"osTree": osT, "p9Tree": p9T}
}

func TestTreeWriteReadRoundTrip(t *testing.T) {
	for name, tr := range treeBackends(t) {
		t.Run(name, func(t *testing.T) {
			if err := tr.WriteFile("a/b/c.txt", []byte("hello"), false); err != nil {
				t.Fatal(err)
			}
			got, err := tr.ReadFile("a/b/c.txt")
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "hello" {
				t.Errorf("got %q", got)
			}
			info, err := tr.Lstat("a/b/c.txt")
			if err != nil {
				t.Fatal(err)
			}
			if !info.Exists || info.IsDir || info.IsSymlink || info.Executable {
				t.Errorf("Lstat = %+v, want a plain non-executable file", info)
			}
		})
	}
}

func TestTreeWriteFileExecutableBit(t *testing.T) {
	for name, tr := range treeBackends(t) {
		t.Run(name, func(t *testing.T) {
			if err := tr.WriteFile("run.sh", []byte("#!/bin/sh\n"), true); err != nil {
				t.Fatal(err)
			}
			info, err := tr.Lstat("run.sh")
			if err != nil {
				t.Fatal(err)
			}
			if !info.Executable {
				t.Error("expected Executable=true")
			}
			// Overwriting toggles the bit even though the path already
			// existed — the case WriteWorkingTree's own doc comment
			// calls out as easy to get wrong (mode only applies at
			// creation on plain open(2)).
			if err := tr.WriteFile("run.sh", []byte("plain"), false); err != nil {
				t.Fatal(err)
			}
			info, err = tr.Lstat("run.sh")
			if err != nil {
				t.Fatal(err)
			}
			if info.Executable {
				t.Error("expected Executable=false after overwriting non-executable")
			}
		})
	}
}

func TestTreeSymlinkRoundTrip(t *testing.T) {
	for name, tr := range treeBackends(t) {
		t.Run(name, func(t *testing.T) {
			if err := tr.WriteFile("target.txt", []byte("real content"), false); err != nil {
				t.Fatal(err)
			}
			if err := tr.Symlink("link", "target.txt"); err != nil {
				t.Fatal(err)
			}
			info, err := tr.Lstat("link")
			if err != nil {
				t.Fatal(err)
			}
			if !info.IsSymlink {
				t.Fatalf("Lstat(link).IsSymlink = false, info = %+v", info)
			}
			if info.SymlinkTarget != "target.txt" {
				t.Errorf("SymlinkTarget = %q, want %q", info.SymlinkTarget, "target.txt")
			}
			// Lstat on the symlink must not follow it — the whole reason
			// this type exists instead of reusing FS.Stat.
			if info.IsDir {
				t.Error("Lstat followed the symlink (IsDir true)")
			}

			// Symlink replaces a stale entry at the same path.
			if err := tr.Symlink("link", "elsewhere"); err != nil {
				t.Fatal(err)
			}
			info, err = tr.Lstat("link")
			if err != nil {
				t.Fatal(err)
			}
			if info.SymlinkTarget != "elsewhere" {
				t.Errorf("SymlinkTarget after replace = %q, want %q", info.SymlinkTarget, "elsewhere")
			}
		})
	}
}

func TestTreeReadDirReportsSymlinksAndExecutable(t *testing.T) {
	for name, tr := range treeBackends(t) {
		t.Run(name, func(t *testing.T) {
			if err := tr.WriteFile("d/plain.txt", []byte("x"), false); err != nil {
				t.Fatal(err)
			}
			if err := tr.WriteFile("d/run.sh", []byte("x"), true); err != nil {
				t.Fatal(err)
			}
			if err := tr.Symlink("d/link", "plain.txt"); err != nil {
				t.Fatal(err)
			}
			entries, err := tr.ReadDir("d")
			if err != nil {
				t.Fatal(err)
			}
			byName := map[string]TreeInfo{}
			for _, e := range entries {
				byName[e.Name] = e.Info
			}
			if len(byName) != 3 {
				t.Fatalf("got %d entries, want 3: %+v", len(byName), byName)
			}
			if byName["plain.txt"].IsSymlink || byName["plain.txt"].Executable {
				t.Errorf("plain.txt: %+v", byName["plain.txt"])
			}
			if !byName["run.sh"].Executable {
				t.Errorf("run.sh: %+v", byName["run.sh"])
			}
			if !byName["link"].IsSymlink || byName["link"].SymlinkTarget != "plain.txt" {
				t.Errorf("link: %+v", byName["link"])
			}
		})
	}
}

func TestTreeRemoveIsIdempotent(t *testing.T) {
	for name, tr := range treeBackends(t) {
		t.Run(name, func(t *testing.T) {
			if err := tr.WriteFile("x", []byte("v"), false); err != nil {
				t.Fatal(err)
			}
			if err := tr.Remove("x"); err != nil {
				t.Fatal(err)
			}
			if err := tr.Remove("x"); err != nil {
				t.Fatalf("removing an already-absent path should be a no-op, got %v", err)
			}
			if info, err := tr.Lstat("x"); err != nil || info.Exists {
				t.Errorf("Lstat after remove = %+v, %v", info, err)
			}
		})
	}
}

func TestTreeMkdirAllThenWrite(t *testing.T) {
	for name, tr := range treeBackends(t) {
		t.Run(name, func(t *testing.T) {
			if err := tr.MkdirAll("a/b/c"); err != nil {
				t.Fatal(err)
			}
			info, err := tr.Lstat("a/b/c")
			if err != nil || !info.Exists || !info.IsDir {
				t.Fatalf("Lstat(a/b/c) = %+v, %v", info, err)
			}
			if err := tr.WriteFile("a/b/c/leaf.txt", []byte("v"), false); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestTreeRefusesIntermediateSymlinkEscape is the actual live bug
// WriteWorkingTree's own doc comment describes, replayed against
// Tree directly: a tracked symlink at "evil" pointing outside the
// tree, plus a second write at "evil/nested/file.txt", must never
// land outside the confined root — on either backend.
func TestTreeRefusesIntermediateSymlinkEscape(t *testing.T) {
	for name, tr := range treeBackends(t) {
		t.Run(name, func(t *testing.T) {
			outside := t.TempDir()
			if err := tr.Symlink("evil", outside); err != nil {
				t.Fatal(err)
			}
			err := tr.WriteFile("evil/nested/file.txt", []byte("escaped"), false)
			if err == nil {
				t.Fatal("expected an error confining the write, got nil")
			}
			if _, statErr := os.Stat(filepath.Join(outside, "nested", "file.txt")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("write escaped the confined root: %v", statErr)
			}
		})
	}
}
