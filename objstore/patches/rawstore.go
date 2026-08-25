package patches

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound is returned when no object with the given hash exists.
var ErrNotFound = errors.New("patches: not found")

// ErrAmbiguous is returned when more than one hash matches a given prefix.
var ErrAmbiguous = errors.New("patches: ambiguous hash prefix")

// rawStore is a content-addressed store of arbitrary byte blobs under one
// directory, fanned out by the first two hex chars of the hash. Both the
// patch store and the binary-blob store are built on this — they differ
// only in what they encode/decode before handing bytes to it.
type rawStore struct {
	dir string
}

func openRaw(dir string) (*rawStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &rawStore{dir: dir}, nil
}

func (s *rawStore) path(h Hash) string {
	hex := h.String()
	return filepath.Join(s.dir, hex[:2], hex[2:])
}

func (s *rawStore) put(data []byte) (Hash, error) {
	h := sha256.Sum256(data)
	path := s.path(h)
	if _, err := os.Stat(path); err == nil {
		return h, nil // already have it
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Hash{}, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o444); err != nil {
		return Hash{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return Hash{}, err
	}
	return h, nil
}

func (s *rawStore) get(h Hash) ([]byte, error) {
	data, err := os.ReadFile(s.path(h))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, h)
	}
	return data, err
}

func (s *rawStore) has(h Hash) bool {
	_, err := os.Stat(s.path(h))
	return err == nil
}

// remove deletes the object stored under h, if present. A no-op, not an
// error, if h was never stored — mirrors os.Remove's ErrNotExist being
// swallowed elsewhere in this package (see openRaw's missing-dir handling
// in spirit): the caller only ever wants "make sure this is gone."
func (s *rawStore) remove(h Hash) error {
	err := os.Remove(s.path(h))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// list returns every hash currently stored, in no particular order. Not
// needed by patches/blobs (content-addressed pull only ever fetches a
// hash it already knows — see vcsfs's dirFile doc comment), but offers
// need real enumeration since browsing the pending queue is the point.
func (s *rawStore) list() ([]Hash, error) {
	fanouts, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Hash
	for _, fanout := range fanouts {
		if !fanout.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.dir, fanout.Name()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				continue
			}
			h, err := HashFromHex(fanout.Name() + e.Name())
			if err != nil {
				continue // not one of ours; skip rather than fail the whole listing
			}
			out = append(out, h)
		}
	}
	return out, nil
}

// resolvePrefix finds the unique stored hash starting with prefix (hex,
// case-insensitive). A full 64-char hex string is returned as-is without
// touching disk; otherwise prefix must be at least 4 hex characters, which
// is enough to pin the on-disk fan-out directory (the first 2 chars)
// unambiguously before scanning it.
func (s *rawStore) resolvePrefix(prefix string) (Hash, error) {
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
