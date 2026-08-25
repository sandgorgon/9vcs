package patches

import (
	"bytes"
	"fmt"
	"testing"
	"time"
)

// TestTopoOrderDeterministicTieBreak: several mutually-independent
// (zero-dependency) patches must come out in strictly ascending hash
// order, regardless of the map iteration order closureOf happened to
// build them in — the same tie-break topoOrder always guaranteed, now
// enforced via a container/heap min-heap (hashHeap) instead of a full
// re-sort of the ready set on every pop.
func TestTopoOrderDeterministicTieBreak(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var hashes []Hash
	for i := range 25 {
		h, err := store.Put(&Patch{Message: fmt.Sprintf("independent-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}

	closure, err := closureOf(store, hashes...)
	if err != nil {
		t.Fatal(err)
	}
	order, err := topoOrder(closure)
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != len(hashes) {
		t.Fatalf("topoOrder returned %d hashes, want %d", len(order), len(hashes))
	}
	for i := 1; i < len(order); i++ {
		if bytes.Compare(order[i-1][:], order[i][:]) >= 0 {
			t.Fatalf("order not strictly ascending at index %d: %s >= %s", i, order[i-1], order[i])
		}
	}
}

// TestTopoOrderManyIndependentPatchesIsFast is the regression test for a
// real O(n² log n) CPU-exhaustion cost: topoOrder used to re-sort its
// entire ready set from scratch on every pop, and a set of many
// mutually-independent (zero-dependency) patches starts with all of them
// in ready at once. Such a set is cheap for an adversarial peer to
// construct (each patch just needs to differ, e.g. by message) and get
// stored via vcsfs's Twalk/Tcreate path or import/reconcile's sync — any
// later Materialize/History/Closure/UniqueChanges call touching them all
// then paid the quadratic cost. The fix (a container/heap min-heap) is
// O(n log n); this asserts a size that would take far longer than this
// bound under the old re-sort-per-pop approach.
func TestTopoOrderManyIndependentPatchesIsFast(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in -short")
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const n = 20_000
	hashes := make([]Hash, n)
	for i := range n {
		h, err := store.Put(&Patch{Message: fmt.Sprintf("independent-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		hashes[i] = h
	}
	closure, err := closureOf(store, hashes...)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan []Hash, 1)
	errc := make(chan error, 1)
	go func() {
		order, err := topoOrder(closure)
		if err != nil {
			errc <- err
			return
		}
		done <- order
	}()
	select {
	case order := <-done:
		if len(order) != n {
			t.Fatalf("topoOrder returned %d hashes, want %d", len(order), n)
		}
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("topoOrder over 20,000 mutually-independent patches did not return within 5s — the ready-set re-sort-per-pop regression appears to be back")
	}
}
