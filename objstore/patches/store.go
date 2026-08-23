package patches

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound is returned by Store.Get when no patch with the given hash exists.
var ErrNotFound = errors.New("patches: not found")

// ErrAmbiguous is returned by Store.ResolvePrefix when more than one patch
// hash matches the given prefix.
var ErrAmbiguous = errors.New("patches: ambiguous hash prefix")

// Store is a content-addressed store of patch objects under one directory
// (repo-relative: .9vcs/patches). Objects are immutable and durable on
// disk — no synthesis layer needed here, unlike /view (see PLAN.md §3).
type Store struct {
	dir string
}

// Open returns a Store rooted at dir, creating it if necessary.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) objectPath(h Hash) string {
	hex := h.String()
	return filepath.Join(s.dir, hex[:2], hex[2:])
}

// Put encodes and hashes p, writing it to the store if not already present.
// Content addressing makes this naturally idempotent.
func (s *Store) Put(p *Patch) (Hash, error) {
	p.SortChanges()
	h := p.Hash()
	path := s.objectPath(h)
	if _, err := os.Stat(path); err == nil {
		return h, nil // already have it
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Hash{}, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, p.Encode(), 0o444); err != nil {
		return Hash{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return Hash{}, err
	}
	return h, nil
}

// Get retrieves the patch with hash h.
func (s *Store) Get(h Hash) (*Patch, error) {
	data, err := os.ReadFile(s.objectPath(h))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, h)
	}
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

// Has reports whether h is present in the store.
func (s *Store) Has(h Hash) bool {
	_, err := os.Stat(s.objectPath(h))
	return err == nil
}

// ResolvePrefix finds the unique stored hash starting with prefix (hex,
// case-insensitive). A full 64-char hex string is returned as-is without
// touching disk; otherwise prefix must be at least 4 hex characters, which
// is enough to pin the on-disk fan-out directory (the first 2 chars)
// unambiguously before scanning it.
func (s *Store) ResolvePrefix(prefix string) (Hash, error) {
	prefix = strings.ToLower(prefix)
	if len(prefix) == 64 {
		return HashFromHex(prefix)
	}
	if len(prefix) < 4 {
		return Hash{}, fmt.Errorf("patches: hash prefix %q too short (need at least 4 hex characters)", prefix)
	}
	entries, err := os.ReadDir(filepath.Join(s.dir, prefix[:2]))
	if errors.Is(err, os.ErrNotExist) {
		return Hash{}, fmt.Errorf("%w: %s", ErrNotFound, prefix)
	}
	if err != nil {
		return Hash{}, err
	}
	rest := prefix[2:]
	var match string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), rest) {
			if match != "" {
				return Hash{}, fmt.Errorf("%w: %s", ErrAmbiguous, prefix)
			}
			match = e.Name()
		}
	}
	if match == "" {
		return Hash{}, fmt.Errorf("%w: %s", ErrNotFound, prefix)
	}
	return HashFromHex(prefix[:2] + match)
}
