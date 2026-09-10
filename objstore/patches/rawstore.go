package patches

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sandgorgon/9vcs/fsx"
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
	fs  fsx.FS
	dir string
}

func openRaw(fs fsx.FS, dir string) (*rawStore, error) {
	if err := fs.MkdirAll(dir); err != nil {
		return nil, err
	}
	return &rawStore{fs: fs, dir: dir}, nil
}

func (s *rawStore) path(h Hash) string {
	hex := h.String()
	return s.fs.Join(s.dir, hex[:2], hex[2:])
}

// put is content-addressed and idempotent: Put failing because the
// path already exists is success here (same content, by construction
// of hashing it here first) — unlike a lock file, which wants that
// same failure to mean contention, not success (see fsx.FS.Lock).
func (s *rawStore) put(data []byte) (Hash, error) {
	h := sha256.Sum256(data)
	if err := s.fs.Put(s.path(h), data); err != nil {
		if errors.Is(err, os.ErrExist) {
			return h, nil
		}
		return Hash{}, err
	}
	return h, nil
}

// get retrieves the object stored under h. The ErrNotFound remapping
// below only reliably fires for an osfs-backed store — base 9P2000 has
// no structured error code to distinguish "not found" from any other
// failure (see PLAN.md's library facts), so a p9fs-backed store's
// not-found case surfaces as whatever raw error the server returned
// instead, a cosmetic rough edge, not a correctness gap.
func (s *rawStore) get(h Hash) ([]byte, error) {
	data, err := s.fs.ReadFile(s.path(h))
	if err != nil {
		if info, statErr := s.fs.Stat(s.path(h)); statErr == nil && !info.Exists {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, h)
		}
		return nil, err
	}
	return data, nil
}

func (s *rawStore) has(h Hash) bool {
	info, _ := s.fs.Stat(s.path(h))
	return info.Exists
}

// remove deletes the object stored under h, if present. A no-op, not
// an error, if h was never stored — fsx.FS.Remove is already
// idempotent this way for osfs; p9fs's Remove refuses unconditionally
// for now (see fsx's package doc).
func (s *rawStore) remove(h Hash) error {
	return s.fs.Remove(s.path(h))
}

// list returns every hash currently stored, in no particular order. Not
// needed by patches/blobs (content-addressed pull only ever fetches a
// hash it already knows — see vcsfs's dirFile doc comment), but offers
// need real enumeration since browsing the pending queue is the point.
func (s *rawStore) list() ([]Hash, error) {
	fanouts, err := s.fs.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Hash
	for _, fanout := range fanouts {
		entries, err := s.fs.ReadDir(s.fs.Join(s.dir, fanout))
		if err != nil {
			continue // not a fanout directory (or vanished); skip it
		}
		for _, e := range entries {
			if strings.HasSuffix(e, ".tmp") {
				continue
			}
			h, err := HashFromHex(fanout + e)
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
	entries, err := s.fs.ReadDir(s.fs.Join(s.dir, prefix[:2]))
	if err != nil {
		return Hash{}, fmt.Errorf("%w: %s", ErrNotFound, prefix)
	}
	rest := prefix[2:]
	var match string
	for _, e := range entries {
		if strings.HasPrefix(e, rest) {
			if match != "" {
				return Hash{}, fmt.Errorf("%w: %s", ErrAmbiguous, prefix)
			}
			match = e
		}
	}
	if match == "" {
		return Hash{}, fmt.Errorf("%w: %s", ErrNotFound, prefix)
	}
	return HashFromHex(prefix[:2] + match)
}
