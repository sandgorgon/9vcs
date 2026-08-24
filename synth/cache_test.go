package synth

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func contents(lines []patches.Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Content
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// put records a single-path text patch against base's current content,
// mirroring objstore/patches' own internal test helper (unexported
// there, so re-implemented here against the exported API this package
// actually depends on).
func put(t *testing.T, store *patches.Store, deps []patches.Hash, path string, content []string, base patches.Index) patches.Hash {
	t.Helper()
	var oldLines []patches.Line
	if st, ok := base[path]; ok && st.Kind == patches.KindText {
		oldLines, _ = patches.Linearize(st.Graph)
	}
	ops, _ := patches.Diff(oldLines, content)
	p := &patches.Patch{Dependencies: deps, Message: "x", Changes: []patches.FileChange{{Path: path, Kind: patches.KindText, Ops: ops, TrailingNewline: true}}}
	h, err := store.Put(p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// removeBackingObject deletes a patch's on-disk object directly — the
// same fanout layout Store uses internally (see
// objstore/patches/rawstore.go) — so a subsequent store.Get(h) fails.
// Tests use this to prove a Cache hit never touches the store: if it
// did, it would fail exactly the way a direct store.Get/Materialize
// call does once the backing object is gone.
func removeBackingObject(t *testing.T, storeDir string, h patches.Hash) {
	t.Helper()
	hex := h.String()
	if err := os.Remove(filepath.Join(storeDir, hex[:2], hex[2:])); err != nil {
		t.Fatalf("removing backing patch object: %v", err)
	}
}

func TestCacheMissMatchesDirectMaterialize(t *testing.T) {
	store, err := patches.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := put(t, store, nil, "f.txt", []string{"one", "two"}, patches.Index{})

	c := NewCache(store)
	cached, err := c.Materialize(h)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := patches.Materialize(store, h)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := patches.Linearize(cached["f.txt"].Graph)
	want, _ := patches.Linearize(direct["f.txt"].Graph)
	if !sameStrings(contents(got), contents(want)) {
		t.Errorf("cached Materialize = %v, want %v (direct)", contents(got), contents(want))
	}
}

// TestCacheHitNeverTouchesStore proves a repeat call is a genuine cache
// hit, not just a correct-but-uncached recomputation: it deletes the
// on-disk patch backing the first call's result before making a second,
// identical call. If the second call had to replay anything at all, it
// would fail (the patch is gone) — succeeding is only possible if it
// came straight from the cache.
func TestCacheHitNeverTouchesStore(t *testing.T) {
	dir := t.TempDir()
	store, err := patches.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := put(t, store, nil, "f.txt", []string{"one", "two"}, patches.Index{})

	c := NewCache(store)
	first, err := c.Materialize(h)
	if err != nil {
		t.Fatal(err)
	}

	removeBackingObject(t, dir, h)
	if _, err := store.Get(h); err == nil {
		t.Fatal("sanity check failed: store.Get should fail after removing the object")
	}

	second, err := c.Materialize(h)
	if err != nil {
		t.Fatalf("second (should-be-cached) Materialize failed, meaning it tried to hit the now-broken store: %v", err)
	}

	got, _ := patches.Linearize(second["f.txt"].Graph)
	want, _ := patches.Linearize(first["f.txt"].Graph)
	if !sameStrings(contents(got), contents(want)) {
		t.Errorf("second (cached) call = %v, want %v (matching the first)", contents(got), contents(want))
	}
}

func TestCacheHitIsOrderIndependent(t *testing.T) {
	dir := t.TempDir()
	store, err := patches.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hA := put(t, store, nil, "a.txt", []string{"a"}, patches.Index{})
	hB := put(t, store, nil, "b.txt", []string{"b"}, patches.Index{})

	c := NewCache(store)
	if _, err := c.Materialize(hA, hB); err != nil {
		t.Fatal(err)
	}

	// Break the store, then call again with the roots in the opposite
	// order: still a hit.
	removeBackingObject(t, dir, hA)
	removeBackingObject(t, dir, hB)

	if _, err := c.Materialize(hB, hA); err != nil {
		t.Fatalf("reordered-roots call should still hit the cache: %v", err)
	}
}

func TestCacheDoesNotCacheErrors(t *testing.T) {
	store, err := patches.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var unknown patches.Hash
	for i := range unknown {
		unknown[i] = 0xCD
	}

	c := NewCache(store)
	if _, err := c.Materialize(unknown); err == nil {
		t.Fatal("expected an error materializing an unknown hash, got nil")
	}

	// An unrelated, valid call afterward must succeed normally — the
	// earlier error must not have left the cache in a bad state.
	h := put(t, store, nil, "f.txt", []string{"one"}, patches.Index{})
	idx, err := c.Materialize(h)
	if err != nil {
		t.Fatalf("Materialize(h) after an unrelated earlier error: %v", err)
	}
	if idx["f.txt"].Kind != patches.KindText {
		t.Errorf("idx[f.txt].Kind = %v, want KindText", idx["f.txt"].Kind)
	}
}
