package main

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func TestSetLocalRefCASAllowsCheckedOutBranch(t *testing.T) {
	r := newTestRepo(t)
	h, err := r.store.Put(&patches.Patch{Message: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Unlike setRefHashCAS, the local variant must be able to move the
	// currently-checked-out branch — that's exactly what record/merge/
	// apply do on every ordinary local command.
	if err := r.setLocalRefCAS(defaultBranch, patches.Hash{}, h); err != nil {
		t.Fatalf("setLocalRefCAS on the checked-out branch: %v", err)
	}
	if got, exists, _ := r.refHash(defaultBranch); !exists || got != h {
		t.Errorf("refs[%s] = %v, %v, want %s, true", defaultBranch, got, exists, h)
	}
}

func TestSetLocalRefCASConflict(t *testing.T) {
	r := newTestRepo(t)
	oldHash, err := r.store.Put(&patches.Patch{Message: "old"})
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := r.store.Put(&patches.Patch{Message: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.setLocalRefCAS("feature", patches.Hash{}, oldHash); err != nil {
		t.Fatal(err)
	}

	if err := r.setLocalRefCAS("feature", newHash /* stale */, newHash); err == nil {
		t.Error("expected a CAS conflict for a stale expected-old, got nil")
	} else if !errors.Is(err, errRefConflict) {
		t.Errorf("error doesn't wrap errRefConflict: %v", err)
	}
	if got, _, _ := r.refHash("feature"); got != oldHash {
		t.Errorf("ref changed despite a rejected CAS write: got %s, want unchanged %s", got, oldHash)
	}

	if err := r.setLocalRefCAS("feature", oldHash, newHash); err != nil {
		t.Fatalf("setLocalRefCAS with the correct expected-old: %v", err)
	}
	if got, _, _ := r.refHash("feature"); got != newHash {
		t.Errorf("refs[feature] = %s, want %s", got, newHash)
	}
}

func TestAtomicWriteFileLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	if err := atomicWriteFile(path, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected no leftover .tmp file, stat err = %v", err)
	}

	// A second write to the same path must fully replace it, not merge
	// with or leave any trace of the first.
	if err := atomicWriteFile(path, []byte("bye")); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "bye" {
		t.Errorf("content after overwrite = %q, want %q", got, "bye")
	}
}

// TestWithRefLockMutualExclusion is the actual concurrency property:
// launch many goroutines all trying to run a critical section (guarded
// by the same repo's withRefLock) that would visibly misbehave under any
// overlap — incrementing a counter, sleeping, then checking nothing else
// incremented it meanwhile. A real race here would show up as
// `inRegion` briefly exceeding 1.
func TestWithRefLockMutualExclusion(t *testing.T) {
	r := newTestRepo(t)
	const n = 20
	var inRegion int32
	var maxObserved int32
	var wg sync.WaitGroup
	var mu sync.Mutex // guards maxObserved only

	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.withRefLock(func() error {
				cur := atomic.AddInt32(&inRegion, 1)
				mu.Lock()
				if cur > maxObserved {
					maxObserved = cur
				}
				mu.Unlock()
				time.Sleep(2 * time.Millisecond)
				atomic.AddInt32(&inRegion, -1)
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if maxObserved != 1 {
		t.Errorf("max concurrent holders observed = %d, want 1 (mutual exclusion violated)", maxObserved)
	}
}

// TestWithRefLockStealsStaleLock confirms a lock file left behind by a
// (simulated) crashed process doesn't deadlock every future ref write
// forever — it has to actually be older than refLockStaleAge, backdated
// here rather than waiting the real interval out.
func TestWithRefLockStealsStaleLock(t *testing.T) {
	r := newTestRepo(t)
	lockPath := r.refLockPath()
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * refLockStaleAge)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	ran := false
	err := r.withRefLock(func() error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("withRefLock should have stolen the stale lock, got: %v", err)
	}
	if !ran {
		t.Error("critical section never ran")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Error("lock file should be removed after a successful withRefLock call")
	}
}

// TestConcurrentSetLocalRefCASOnlyOneWins is the actual race this whole
// feature exists to close: many local commands (goroutines standing in
// for separate process invocations) all read the same "old" ref value
// and race to move it to their own distinct new value. Before this fix,
// setRefHash was a blind, unconditional os.WriteFile — whichever write
// landed last would have silently won, discarding every other one's
// patch from the branch with no error raised anywhere. With CAS +
// locking, exactly one must succeed and every other must fail loudly.
func TestConcurrentSetLocalRefCASOnlyOneWins(t *testing.T) {
	r := newTestRepo(t)
	base, err := r.store.Put(&patches.Patch{Message: "base"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.setLocalRefCAS("contended", patches.Hash{}, base); err != nil {
		t.Fatal(err)
	}

	const n = 10
	candidates := make([]patches.Hash, n)
	for i := range n {
		h, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "candidate"})
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
			results[i] = r.setLocalRefCAS("contended", base, candidates[i])
		}(i)
	}
	wg.Wait()

	successes := 0
	var winner patches.Hash
	for i, err := range results {
		if err == nil {
			successes++
			winner = candidates[i]
		} else if !errors.Is(err, errRefConflict) {
			t.Errorf("candidate %d failed with a non-conflict error: %v", i, err)
		}
	}
	if successes != 1 {
		t.Fatalf("%d of %d concurrent writers succeeded, want exactly 1", successes, n)
	}
	if got, _, _ := r.refHash("contended"); got != winner {
		t.Errorf("final ref = %s, want the one writer that actually succeeded (%s)", got, winner)
	}
}
