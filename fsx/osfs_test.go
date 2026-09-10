package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOSFSReadWriteRoundTrip(t *testing.T) {
	fs := NewOS(t.TempDir())
	if err := fs.Put("a/b/c.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile("a/b/c.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestOSFSPutFailsIfAlreadyExists(t *testing.T) {
	fs := NewOS(t.TempDir())
	if err := fs.Put("x", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	err := fs.Put("x", []byte("v2"))
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("second Put at the same path: got %v, want an error satisfying errors.Is(err, os.ErrExist)", err)
	}
	got, err := fs.ReadFile("x")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v1" {
		t.Errorf("content changed despite the failed second create: got %q", got)
	}
}

func TestOSFSLockExclusivity(t *testing.T) {
	fs := NewOS(t.TempDir())
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

func TestOSFSWriteAtomicReplacesContent(t *testing.T) {
	fs := NewOS(t.TempDir())
	if err := fs.WriteAtomic("ref", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteAtomic("ref", []byte("new")); err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile("ref")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("got %q, want %q", got, "new")
	}
}

func TestOSFSWriteAtomicNoTornRead(t *testing.T) {
	root := t.TempDir()
	fs := NewOS(root)
	// Prime a large-ish value so a naive in-place overwrite would have a
	// visible window where the file is shorter than either value.
	old := make([]byte, 4096)
	for i := range old {
		old[i] = 'o'
	}
	newVal := make([]byte, 4096)
	for i := range newVal {
		newVal[i] = 'n'
	}
	if err := fs.WriteAtomic("ref", old); err != nil {
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
				continue // absent mid-rename is fine, never returned by osfs's atomic rename anyway
			}
			if len(got) != len(old) {
				readErr = errors.New("observed a torn/partial write")
				return
			}
		}
	})
	for i := range 200 {
		v := old
		if i%2 == 1 {
			v = newVal
		}
		if err := fs.WriteAtomic("ref", v); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if readErr != nil {
		t.Fatal(readErr)
	}
}

func TestOSFSStatAbsentIsNoError(t *testing.T) {
	fs := NewOS(t.TempDir())
	info, err := fs.Stat("nope")
	if err != nil {
		t.Fatalf("Stat of an absent path should not error, got %v", err)
	}
	if info.Exists {
		t.Error("expected Exists=false")
	}
}

func TestOSFSMkdirAllThenReadDir(t *testing.T) {
	fs := NewOS(t.TempDir())
	if err := fs.MkdirAll("a/b"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Put("a/b/one", []byte("1")); err != nil {
		t.Fatal(err)
	}
	names, err := fs.ReadDir("a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "one" {
		t.Errorf("got %v", names)
	}
}

func TestOSFSRemoveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	fs := NewOS(dir)
	if err := fs.Put("x", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove("x"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Remove("x"); err != nil {
		t.Fatalf("removing an already-absent path should be a no-op, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); !os.IsNotExist(err) {
		t.Errorf("expected the file to actually be gone")
	}
}
