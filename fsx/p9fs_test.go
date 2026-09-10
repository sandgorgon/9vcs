package fsx

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sandgorgon/9p/client"
	"github.com/sandgorgon/9p/examples/dirfs"
	"github.com/sandgorgon/9p/server"
)

// dialDirfs serves root over an in-process 9P connection backed by
// dirfs — exactly what 9sh's /local binding uses in production (see
// PLAN.md decision #9) — and returns a client already Attach'd to it.
func dialDirfs(t *testing.T, root string) *client.Client {
	t.Helper()
	fs, err := dirfs.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &server.Server{FS: fs}
	go srv.Serve(ln)

	c, err := client.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Attach("test", ""); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestP9FSReadFileAndReadDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "b", "c.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := dialDirfs(t, root)
	fs := NewP9(c, "")

	got, err := fs.ReadFile("a/b/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}

	names, err := fs.ReadDir("a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "c.txt" {
		t.Errorf("got %v", names)
	}
}

func TestP9FSRootedSubpath(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "local", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "local", "repo", "f"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := dialDirfs(t, root)
	// Mirrors ResolveRoot's "local"-rooted case: an FS rooted at a
	// subpath already walked within the namespace.
	fs := NewP9(c, "local/repo")

	got, err := fs.ReadFile("f")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v" {
		t.Errorf("got %q", got)
	}
}

func TestP9FSStat(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := dialDirfs(t, root)
	fs := NewP9(c, "")

	if info, err := fs.Stat("f"); err != nil || !info.Exists || info.IsDir {
		t.Errorf("Stat(f) = %+v, %v", info, err)
	}
	if info, err := fs.Stat("d"); err != nil || !info.Exists || !info.IsDir {
		t.Errorf("Stat(d) = %+v, %v", info, err)
	}
	if info, err := fs.Stat("nope"); err != nil || info.Exists {
		t.Errorf("Stat(nope) = %+v, %v, want Exists=false, err=nil", info, err)
	}
}

func TestP9FSMkdirAllCreatesMissingSegments(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := dialDirfs(t, root)
	fs := NewP9(c, "")

	if err := fs.MkdirAll("d"); err != nil {
		t.Errorf("MkdirAll of an already-existing directory should succeed, got %v", err)
	}
	if err := fs.MkdirAll("d/nested/deeper"); err != nil {
		t.Fatalf("MkdirAll of missing nested segments: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "d", "nested", "deeper"))
	if err != nil || !info.IsDir() {
		t.Errorf("expected d/nested/deeper to exist as a real directory, got %v, %v", info, err)
	}
}

func TestP9FSPutReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	c := dialDirfs(t, root)
	fs := NewP9(c, "")

	if err := fs.Put("ab/cdef", []byte("patch content")); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile("ab/cdef")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "patch content" {
		t.Errorf("got %q", got)
	}

	// No leftover temp file next to it.
	names, err := fs.ReadDir("ab")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "cdef" {
		t.Errorf("directory listing after Put: got %v, want just [cdef]", names)
	}

	// Idempotent: a second Put with the same content lands cleanly
	// (dirfs's WStat-based rename overwrites rather than failing, which
	// is fine here since content-addressed callers only ever call this
	// with the correct content for path — see Put's doc comment).
	if err := fs.Put("ab/cdef", []byte("patch content")); err != nil {
		t.Fatalf("second identical Put should succeed, got %v", err)
	}
}

func TestP9FSWriteAtomicReplacesContentNoTornRead(t *testing.T) {
	root := t.TempDir()
	c := dialDirfs(t, root)
	fs := NewP9(c, "")

	old := strings.Repeat("o", 4096)
	newVal := strings.Repeat("n", 4096)
	if err := fs.WriteAtomic("ref", []byte(old)); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var readErr error
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := fs.ReadFile("ref")
			if err != nil {
				continue // absent mid-publish is fine, never returned here anyway
			}
			if len(got) != len(old) {
				readErr = errors.New("observed a torn/partial write")
				return
			}
		}
	})
	for i := range 50 {
		v := old
		if i%2 == 1 {
			v = newVal
		}
		if err := fs.WriteAtomic("ref", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if readErr != nil {
		t.Fatal(readErr)
	}
	got, err := fs.ReadFile("ref")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != newVal {
		t.Errorf("final content = %q, want the last value written", got)
	}
}

func TestP9FSLockExclusivity(t *testing.T) {
	root := t.TempDir()
	c := dialDirfs(t, root)
	fs := NewP9(c, "")

	if err := fs.Lock("lock"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Lock("lock"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second Lock while held: got %v, want an error satisfying errors.Is(err, os.ErrExist)", err)
	}
	if err := fs.Remove("lock"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Lock("lock"); err != nil {
		t.Fatalf("Lock after release should succeed, got %v", err)
	}
}

func TestP9FSRemoveIsIdempotent(t *testing.T) {
	root := t.TempDir()
	c := dialDirfs(t, root)
	fs := NewP9(c, "")

	if err := fs.Put("x", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove("x"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove("x"); err != nil {
		t.Fatalf("removing an already-absent path should be a no-op, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "x")); !os.IsNotExist(err) {
		t.Errorf("expected the file to actually be gone")
	}
}
