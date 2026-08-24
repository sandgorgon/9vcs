package patches

// Store is a content-addressed store of patch objects under one directory
// (repo-relative: .9vcs/patches). Objects are immutable and durable on
// disk — no synthesis layer needed here, unlike /view (see PLAN.md §3).
type Store struct {
	raw *rawStore
}

// Open returns a Store rooted at dir, creating it if necessary.
func Open(dir string) (*Store, error) {
	raw, err := openRaw(dir)
	if err != nil {
		return nil, err
	}
	return &Store{raw: raw}, nil
}

// Put encodes and hashes p, writing it to the store if not already present.
// Content addressing makes this naturally idempotent.
func (s *Store) Put(p *Patch) (Hash, error) {
	p.Normalize()
	return s.raw.put(p.Encode())
}

// Get retrieves the patch with hash h.
func (s *Store) Get(h Hash) (*Patch, error) {
	data, err := s.raw.get(h)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

// GetRaw returns the exact encoded bytes stored for h, without decoding —
// what a 9P server serves verbatim (see vcsfs), and what a peer receiving
// them can Decode and re-Put to both validate and store in one step.
func (s *Store) GetRaw(h Hash) ([]byte, error) { return s.raw.get(h) }

// Has reports whether h is present in the store.
func (s *Store) Has(h Hash) bool { return s.raw.has(h) }

// ResolvePrefix finds the unique stored hash starting with prefix. See
// rawStore.resolvePrefix for the matching rules.
func (s *Store) ResolvePrefix(prefix string) (Hash, error) { return s.raw.resolvePrefix(prefix) }
