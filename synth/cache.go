// Package synth is the materialization cache named in PLAN.md's module
// layout: a thin, in-memory memoizing wrapper around
// objstore/patches.Materialize (the actual replay engine, which ended up
// living in objstore/patches itself rather than here — see PLAN.md's
// Status for that divergence from the original proposal). Intended to be
// shared by every consumer that replays the same roots more than once:
// today that's a single CLI command computing overlapping closures (a
// merge preview materializes ours, theirs, and their union all in one
// invocation), and eventually `serve --view`'s live namespace, per
// PLAN.md's "synth/ ... shared by checkout ... and serve --view".
package synth

import (
	"bytes"
	"sort"
	"strings"
	"sync"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// Cache memoizes patches.Materialize by its roots. Patches are immutable
// and content-addressed (see objstore/patches/store.go): Materialize is
// a pure function of its roots alone, and closureOf/topoOrder don't
// depend on the order roots were passed in (both reduce to a set before
// doing anything order-sensitive) — so a cache hit needs no
// invalidation whatsoever, only a stable, order-independent key. There
// is no eviction: fine for a single short-lived CLI command (today's
// only real caller), a real concern for a long-running `serve --view`
// once that exists — noted here rather than solved speculatively now.
type Cache struct {
	store *patches.Store

	mu    sync.Mutex
	byKey map[string]patches.Index
}

// NewCache wraps store with a fresh, empty cache.
func NewCache(store *patches.Store) *Cache {
	return &Cache{store: store, byKey: map[string]patches.Index{}}
}

// Materialize is patches.Materialize(store, roots...), memoized: a
// repeat call with the same roots, in any order, returns the same Index
// without touching the store or replaying anything. A failed call is
// never cached, so a transient error doesn't poison later retries.
func (c *Cache) Materialize(roots ...patches.Hash) (patches.Index, error) {
	key := rootsKey(roots)

	c.mu.Lock()
	if idx, ok := c.byKey[key]; ok {
		c.mu.Unlock()
		return idx, nil
	}
	c.mu.Unlock()

	idx, err := patches.Materialize(c.store, roots...)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.byKey[key] = idx
	c.mu.Unlock()
	return idx, nil
}

// rootsKey normalizes roots into an order-independent map key: sorted,
// since two calls that pass the same set of roots in a different order
// (e.g. Materialize(ours, theirs) vs. Materialize(theirs, ours)) are
// asking for and must get the exact same cache entry.
func rootsKey(roots []patches.Hash) string {
	sorted := append([]patches.Hash(nil), roots...)
	sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i][:], sorted[j][:]) < 0 })
	var b strings.Builder
	for _, h := range sorted {
		b.WriteString(h.String())
		b.WriteByte(',')
	}
	return b.String()
}
