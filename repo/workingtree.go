package repo

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// binaryProbeBytes caps how much of a file IsBinary inspects, matching
// git's own heuristic size.
const binaryProbeBytes = 8000

// IsBinary reports whether content looks like binary data: a NUL byte
// within the first binaryProbeBytes is the standard, cheap heuristic (same
// one git uses) — text files essentially never contain one.
func IsBinary(content []byte) bool {
	n := min(len(content), binaryProbeBytes)
	return bytes.IndexByte(content[:n], 0) != -1
}

func HasTrailingNewline(content []byte) bool {
	return len(content) > 0 && content[len(content)-1] == '\n'
}

// SplitLines splits s into lines without the trailing newline, matching how
// Diff compares against Line.Content. Whether a trailing newline was
// present at all is tracked separately (HasTrailingNewline) so JoinLines
// can reproduce the file byte-for-byte.
func SplitLines(s string) []string {
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

// JoinLines is SplitLines's inverse, given whether the original content
// ended in a newline. Getting this wrong silently appends a stray byte on
// every checkout — harmless for most text files, but real corruption for
// anything without one, binary content especially.
func JoinLines(lines []patches.Line, trailingNewline bool) string {
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

// ChangedFiles compares the working tree against base (a materialized
// Index — possibly a merge union, with forks), returning the FileChange
// for every path that differs — added, removed, or edited, text or binary
// — keyed by path.
//
// A path matching .9vcsignore is skipped entirely, but only when it isn't
// already in base: an ignore pattern only ever suppresses a genuinely new,
// untracked file from being swept in, exactly like .gitignore never
// un-tracks a file git already knows about. That's why the check happens
// here, against base, rather than as a filter inside WorkingFiles — that
// method has no way to tell "new" from "already tracked" apart, and
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
func ChangedFiles(r *Repo, base patches.Index) (map[string]patches.FileChange, error) {
	if r.Tree == nil {
		return nil, ErrWorkingTreeUnsupported
	}
	paths, err := r.WorkingFiles()
	if err != nil {
		return nil, err
	}
	ignore, err := LoadIgnore(r.Tree)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", IgnoreFileName, err)
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
		info, err := r.Tree.Lstat(p)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}

		if info.IsSymlink {
			target := info.SymlinkTarget
			if existed && prior.Kind == patches.KindSymlink && prior.SymlinkTarget == target {
				continue // unchanged
			}
			out[p] = patches.FileChange{Path: p, Kind: patches.KindSymlink, SymlinkTarget: target}
			continue
		}

		executable := info.Executable
		content, err := r.Tree.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}

		if IsBinary(content) {
			hash, err := r.Blobs.Put(content)
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
		trailing := HasTrailingNewline(content)
		ops, finalLines := patches.Diff(patches.StripMarkers(baseLines), SplitLines(string(content)))
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

// WriteWorkingTree materializes new to disk, then removes any file present
// in old but absent from new (i.e. deleted between the two points). A
// KindText path with unresolved forks is written with inline conflict
// markers — the presented rendering, not a resolved one.
//
// Every file operation goes through r.Tree, not a plain path join
// followed by a bare filesystem call — a real, live-proven vulnerability
// otherwise: a tracked symlink used as an *intermediate* path component
// (e.g. a change at "evil" — a symlink pointing outside the repo — plus
// a second change at "evil/nested/file.txt") causes a naive write to
// follow the symlink at the OS level and write completely outside the
// repo, escaping the earlier fix for literal ".."-style paths entirely
// (the path string "evil/nested/file.txt" is perfectly canonical — the
// escape happens via what's *already sitting on disk*, not via the
// string). fsx.Tree confines every operation to stay under the working
// tree root (see its doc comment for what that guarantee actually is on
// each backend) — while still allowing an absolute target to be
// *created* as a leaf symlink (creating one doesn't need to resolve
// where it points), so a legitimate case like "bin/env -> /usr/bin/env"
// keeps working. See PLAN.md's "Symlink path traversal via an
// intermediate component" for the full writeup.
func WriteWorkingTree(r *Repo, old, new patches.Index) error {
	if r.Tree == nil {
		return ErrWorkingTreeUnsupported
	}
	tree := r.Tree

	for p, st := range new {
		if st.Kind == patches.KindSymlink {
			if err := tree.Symlink(p, st.SymlinkTarget); err != nil {
				return fmt.Errorf("creating symlink %s: %w", p, err)
			}
			continue
		}
		// If a symlink currently occupies this path and the new content
		// isn't itself a symlink, clear it first — WriteFile doesn't
		// follow a symlink at its own leaf, and would otherwise fail (or,
		// worse, on a backend that did follow it, clobber whatever it
		// points to instead of replacing the tracked path).
		if info, err := tree.Lstat(p); err == nil && info.IsSymlink {
			if err := tree.Remove(p); err != nil {
				return fmt.Errorf("removing stale symlink at %s: %w", p, err)
			}
		}

		var content []byte
		switch st.Kind {
		case patches.KindBlob:
			data, err := r.Blobs.Get(st.Blob)
			if err != nil {
				return fmt.Errorf("reading blob for %s: %w", p, err)
			}
			content = data
		default:
			lines, _ := patches.Linearize(st.Graph)
			content = []byte(JoinLines(lines, st.TrailingNewline))
		}
		if err := tree.WriteFile(p, content, st.Executable); err != nil {
			return fmt.Errorf("writing %s: %w", p, err)
		}
	}
	for p := range old {
		if _, ok := new[p]; ok {
			continue
		}
		if err := tree.Remove(p); err != nil {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	return nil
}

// SortedPaths returns m's keys, sorted — the deterministic display order
// every ChangedFiles consumer (CLI or external) wants when iterating its
// map.
func SortedPaths(m map[string]patches.FileChange) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
