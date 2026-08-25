package main

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

// author resolves this record's Author string: "Name <email>" if both
// user.name and user.email are configured (see resolvedAuthorField for
// the repo-local-then-global precedence), "Name" alone if only
// user.name is, or the OS username if neither is configured — unchanged
// fallback, so a fresh, unconfigured install behaves exactly as before.
// A malformed config file (hand-edited, since `9vcs config` itself never
// writes one) is a real error here rather than a silent fallback — it's
// a user mistake worth surfacing, not an incidental environment failure.
func author(r *repo) (string, error) {
	name, err := resolvedAuthorField(r, "user.name")
	if err != nil {
		return "", fmt.Errorf("resolving user.name: %w", err)
	}
	email, err := resolvedAuthorField(r, "user.email")
	if err != nil {
		return "", fmt.Errorf("resolving user.email: %w", err)
	}
	return formatAuthor(name, email), nil
}

// formatAuthor is author's pure formatting step, split out so it's
// testable without touching any config file or the real OS/global
// config directory: "Name <email>" if both are set, "Name" alone if
// only name is, otherwise the OS username fallback.
func formatAuthor(name, email string) string {
	switch {
	case name != "" && email != "":
		return fmt.Sprintf("%s <%s>", name, email)
	case name != "":
		return name
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

// signPatch signs patch with this install's identity, in place, before
// record stores it: sets AuthorFingerprint to the public key and
// AuthorSignature over patch.SignablePayload(). identity.Load() failing
// (permissions, disk full, whatever) leaves patch unsigned — a warning
// on stderr, not a failed record. record is the single most-invoked
// command; blocking it on an unrelated identity problem for a field
// that's opportunistic, not required, would be a real regression. An
// unsigned patch is a fully legitimate state — see
// Patch.VerifyAuthorSignature.
//
// Normalizes patch first — this is load-bearing, not just tidiness:
// Store.Put also calls Normalize (sorting Dependencies/Changes) right
// before Encode, so signing an un-normalized patch computes a signature
// over different bytes than what's actually stored and later verified,
// the moment there's more than one Dependency or Change to reorder — a
// two-way merge's Changes usually didn't expose this (often zero or one
// entry), but a real N-way apply's multi-dependency merge patch did,
// caught by live testing (Fingerprint showed "INVALID SIGNATURE" on a
// clean three-way apply). Normalize is idempotent, so Store.Put's own
// call afterward is a harmless no-op.
func signPatch(patch *patches.Patch) {
	patch.Normalize()
	id, err := identity.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: recording unsigned (identity unavailable): %v\n", err)
		return
	}
	copy(patch.AuthorFingerprint[:], id.Key.Public().(ed25519.PublicKey))
	sig := ed25519.Sign(id.Key, patch.SignablePayload())
	copy(patch.AuthorSignature[:], sig)
}

// binaryProbeBytes caps how much of a file isBinary inspects, matching
// git's own heuristic size.
const binaryProbeBytes = 8000

// isBinary reports whether content looks like binary data: a NUL byte
// within the first binaryProbeBytes is the standard, cheap heuristic (same
// one git uses) — text files essentially never contain one.
func isBinary(content []byte) bool {
	n := min(len(content), binaryProbeBytes)
	return bytes.IndexByte(content[:n], 0) != -1
}

func hasTrailingNewline(content []byte) bool {
	return len(content) > 0 && content[len(content)-1] == '\n'
}

// splitLines splits s into lines without the trailing newline, matching how
// Diff compares against Line.Content. Whether a trailing newline was
// present at all is tracked separately (hasTrailingNewline) so joinLines
// can reproduce the file byte-for-byte.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	if s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
	}
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	lines = append(lines, s[start:])
	return lines
}

// joinLines is splitLines's inverse, given whether the original content
// ended in a newline. Getting this wrong silently appends a stray byte on
// every checkout — harmless for most text files, but real corruption for
// anything without one, binary content especially.
func joinLines(lines []patches.Line, trailingNewline bool) string {
	var buf strings.Builder
	for i, l := range lines {
		if i > 0 {
			buf.WriteByte('\n')
		}
		buf.WriteString(l.Content)
	}
	if trailingNewline && len(lines) > 0 {
		buf.WriteByte('\n')
	}
	return buf.String()
}

// changedFiles compares the working tree against base (a materialized
// Index — possibly a merge union, with forks), returning the FileChange
// for every path that differs — added, removed, or edited, text or binary
// — keyed by path.
//
// For a path whose base has unresolved forks, this is also where
// conflict resolution actually happens: the working-tree content is
// diffed against the fork's marker-stripped rendering (never the
// marker-included one — see patches.StripMarkers), and Resolve's healing
// ops are folded in automatically. Every caller — record, diff, and
// checkout's dirty-tree check — gets this for free without needing to
// know whether a merge is in progress.
func changedFiles(r *repo, base patches.Index) (map[string]patches.FileChange, error) {
	paths, err := r.workingFiles()
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(paths))
	for _, p := range paths {
		present[p] = true
	}

	out := map[string]patches.FileChange{}
	for _, p := range paths {
		content, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(p)))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		prior, existed := base[p]

		if isBinary(content) {
			hash, err := r.blobs.Put(content)
			if err != nil {
				return nil, fmt.Errorf("storing blob for %s: %w", p, err)
			}
			if existed && prior.Kind == patches.KindBlob && prior.Blob == hash {
				continue // unchanged
			}
			out[p] = patches.FileChange{Path: p, Kind: patches.KindBlob, Blob: hash}
			continue
		}

		var baseLines []patches.Line
		var forks []patches.Fork
		if prior.Kind == patches.KindText && prior.Graph != nil {
			baseLines, forks = patches.Linearize(prior.Graph)
		}
		trailing := hasTrailingNewline(content)
		ops, finalLines := patches.Diff(patches.StripMarkers(baseLines), splitLines(string(content)))
		if len(forks) > 0 {
			ops = append(ops, patches.Resolve(forks, finalLines)...)
		}
		unchanged := existed && prior.Kind == patches.KindText && len(forks) == 0 &&
			len(ops) == 0 && prior.TrailingNewline == trailing
		if unchanged {
			continue
		}
		out[p] = patches.FileChange{Path: p, Kind: patches.KindText, Ops: ops, TrailingNewline: trailing}
	}

	for p := range base {
		if present[p] {
			continue
		}
		out[p] = patches.FileChange{Path: p, Kind: patches.KindDelete}
	}
	return out, nil
}

// writeWorkingTree materializes new to disk, then removes any file present
// in old but absent from new (i.e. deleted between the two points). A
// KindText path with unresolved forks is written with inline conflict
// markers — the presented rendering, not a resolved one.
func writeWorkingTree(r *repo, old, new patches.Index) error {
	for p, st := range new {
		full := filepath.Join(r.root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		var content []byte
		switch st.Kind {
		case patches.KindBlob:
			data, err := r.blobs.Get(st.Blob)
			if err != nil {
				return fmt.Errorf("reading blob for %s: %w", p, err)
			}
			content = data
		default:
			lines, _ := patches.Linearize(st.Graph)
			content = []byte(joinLines(lines, st.TrailingNewline))
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			return err
		}
	}
	for p := range old {
		if _, ok := new[p]; ok {
			continue
		}
		full := filepath.Join(r.root, filepath.FromSlash(p))
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func sortedPaths(m map[string]patches.FileChange) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
