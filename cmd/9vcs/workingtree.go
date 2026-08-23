package main

import (
	"bytes"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func author() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
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
// Index), returning the FileChange for every path that differs — added,
// removed, or edited, text or binary — keyed by path.
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

		trailing := hasTrailingNewline(content)
		var priorLines []patches.Line
		if prior.Kind == patches.KindText {
			priorLines = prior.Lines
		}
		ops, _ := patches.Diff(priorLines, splitLines(string(content)))
		if existed && prior.Kind == patches.KindText && len(ops) == 0 && prior.TrailingNewline == trailing {
			continue // unchanged
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
// in old but absent from new (i.e. deleted between the two points).
func writeWorkingTree(r *repo, old, new patches.Index) error {
	for p, st := range new {
		full := filepath.Join(r.root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		var content []byte
		if st.Kind == patches.KindBlob {
			data, err := r.blobs.Get(st.Blob)
			if err != nil {
				return fmt.Errorf("reading blob for %s: %w", p, err)
			}
			content = data
		} else {
			content = []byte(joinLines(st.Lines, st.TrailingNewline))
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
