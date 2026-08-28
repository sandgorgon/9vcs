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

	"github.com/sandgorgon/9auth"
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
// AuthorSignature over patch.SignablePayload(). auth.Load() failing
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
	id, err := auth.Load()
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
// A path matching .9vcsignore is skipped entirely, but only when it isn't
// already in base: an ignore pattern only ever suppresses a genuinely new,
// untracked file from being swept in, exactly like .gitignore never
// un-tracks a file git already knows about. That's why the check happens
// here, against base, rather than as a filter inside workingFiles — that
// function has no way to tell "new" from "already tracked" apart, and
// getting this backwards would make ignoring a directory after something
// inside it was already recorded silently look like a deletion.
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
	ignore, err := loadIgnore(r.root)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ignoreFileName, err)
	}
	present := make(map[string]bool, len(paths))
	for _, p := range paths {
		present[p] = true
	}

	out := map[string]patches.FileChange{}
	for _, p := range paths {
		prior, existed := base[p]
		if !existed && ignore.matches(p) {
			continue
		}
		full := filepath.Join(r.root, filepath.FromSlash(p))
		info, err := os.Lstat(full)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return nil, fmt.Errorf("reading symlink %s: %w", p, err)
			}
			if existed && prior.Kind == patches.KindSymlink && prior.SymlinkTarget == target {
				continue // unchanged
			}
			out[p] = patches.FileChange{Path: p, Kind: patches.KindSymlink, SymlinkTarget: target}
			continue
		}

		executable := info.Mode()&0o111 != 0
		content, err := os.ReadFile(full)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}

		if isBinary(content) {
			hash, err := r.blobs.Put(content)
			if err != nil {
				return nil, fmt.Errorf("storing blob for %s: %w", p, err)
			}
			if existed && prior.Kind == patches.KindBlob && prior.Blob == hash && prior.Executable == executable {
				continue // unchanged
			}
			out[p] = patches.FileChange{Path: p, Kind: patches.KindBlob, Blob: hash, Executable: executable}
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
			len(ops) == 0 && prior.TrailingNewline == trailing && prior.Executable == executable
		if unchanged {
			continue
		}
		out[p] = patches.FileChange{Path: p, Kind: patches.KindText, Ops: ops, TrailingNewline: trailing, Executable: executable}
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
//
// Every file operation goes through an os.Root rooted at r.root, not a
// plain filepath.Join followed by a bare os.* call — a real,
// live-proven vulnerability otherwise: a tracked symlink used as an
// *intermediate* path component (e.g. a change at "evil" — a symlink
// pointing outside the repo — plus a second change at
// "evil/nested/file.txt") causes a plain os.MkdirAll/os.WriteFile to
// follow the symlink at the OS level and write completely outside the
// repo, escaping the earlier fix for literal ".."-style paths entirely
// (the path string "evil/nested/file.txt" is perfectly canonical — the
// escape happens via what's *already sitting on disk*, not via the
// string). os.Root confines every operation to stay under r.root: it
// follows a symlink that stays within the root, but refuses one that
// would leave it (and refuses an absolute-target symlink as an
// intermediate component outright) — while still allowing an absolute
// target to be *created* as a leaf symlink (creating one doesn't need
// to resolve where it points), so a legitimate case like
// "bin/env -> /usr/bin/env" keeps working. See PLAN.md's "Symlink path
// traversal via an intermediate component" for the full writeup.
func writeWorkingTree(r *repo, old, new patches.Index) error {
	root, err := os.OpenRoot(r.root)
	if err != nil {
		return fmt.Errorf("opening working tree root: %w", err)
	}
	defer root.Close()

	for p, st := range new {
		rel := filepath.FromSlash(p)
		if err := root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			return err
		}

		if st.Kind == patches.KindSymlink {
			// A plain Symlink would fail outright if a regular file (or
			// a stale symlink to something else) already sits here —
			// clear it first. ENOENT (nothing there yet) is fine.
			if err := root.Remove(rel); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("clearing %s before creating symlink: %w", p, err)
			}
			if err := root.Symlink(st.SymlinkTarget, rel); err != nil {
				return fmt.Errorf("creating symlink %s: %w", p, err)
			}
			continue
		}
		// If a symlink currently occupies this path and the new content
		// isn't itself a symlink, remove it first — WriteFile would
		// otherwise follow it (if it resolves within the root at all;
		// os.Root refuses it outright if not) and clobber whatever it
		// points to, instead of replacing the tracked path.
		if fi, err := root.Lstat(rel); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := root.Remove(rel); err != nil {
				return fmt.Errorf("removing stale symlink at %s: %w", p, err)
			}
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
		mode := os.FileMode(0o644)
		if st.Executable {
			mode = 0o755
		}
		if err := root.WriteFile(rel, content, mode); err != nil {
			return err
		}
		// WriteFile's mode argument only applies when it actually
		// creates the file — POSIX open(2) leaves an existing file's
		// permission bits untouched even with O_CREAT — so an existing
		// path (the common case: checkout overwriting what's already
		// there) needs an explicit chmod or a toggled executable bit
		// would silently fail to take effect.
		if err := root.Chmod(rel, mode); err != nil {
			return fmt.Errorf("setting mode for %s: %w", p, err)
		}
	}
	for p := range old {
		if _, ok := new[p]; ok {
			continue
		}
		if err := root.Remove(filepath.FromSlash(p)); err != nil && !os.IsNotExist(err) {
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
