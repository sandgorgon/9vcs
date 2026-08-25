package patches

import (
	"sync"
	"testing"
)

// TestConcurrentPutSameContentDoesNotError is the regression test for a
// real, live-proven race: rawStore.put used a temp filename derived
// only from the content hash (path+".tmp"), so two concurrent put calls
// for the *same* content (same hash — a real, reachable scenario: two
// peer connections relaying the same patch to one `serve` process at
// once, each its own goroutine) could open/create/rename the shared
// temp path out from under each other. Observed live as a spurious
// "permission denied" before the fix, on an otherwise completely
// harmless race (the content two concurrent callers write is identical
// by construction, since it's keyed by its own hash).
func TestConcurrentPutSameContentDoesNotError(t *testing.T) {
	for attempt := range 5 {
		store, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		const n = 20
		var wg sync.WaitGroup
		hashes := make([]Hash, n)
		errs := make([]error, n)
		for i := range n {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				hashes[i], errs[i] = store.Put(&Patch{Message: "shared"})
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("attempt %d goroutine %d: %v", attempt, i, err)
			}
			if hashes[i] != hashes[0] {
				t.Errorf("attempt %d goroutine %d: hash %s, want %s (all writing identical content)", attempt, i, hashes[i], hashes[0])
			}
		}
		if !store.Has(hashes[0]) {
			t.Errorf("attempt %d: store should have the patch after all writers succeeded", attempt)
		}
	}
}
