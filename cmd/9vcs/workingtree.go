package main

import (
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

// splitLines splits s into lines without the trailing newline, matching how
// Diff compares against Line.Content. A trailing newline (the normal case)
// does not produce a spurious empty final line; a file with no trailing
// newline still gets its last, incomplete line diffed correctly.
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

// joinLines is splitLines's inverse: one line per line, newline-terminated.
func joinLines(lines []patches.Line) string {
	var buf strings.Builder
	for _, l := range lines {
		buf.WriteString(l.Content)
		buf.WriteByte('\n')
	}
	return buf.String()
}

// changedFiles compares the working tree against base (a materialized
// Index), returning the diff ops for every path that differs — added,
// removed, or edited — keyed by path.
func changedFiles(r *repo, base patches.Index) (map[string][]patches.LineOp, error) {
	paths, err := r.workingFiles()
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(paths))
	for _, p := range paths {
		present[p] = true
	}

	out := map[string][]patches.LineOp{}
	for _, p := range paths {
		content, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(p)))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		ops, _ := patches.Diff(base[p], splitLines(string(content)))
		if len(ops) > 0 {
			out[p] = ops
		}
	}
	for p, lines := range base {
		if present[p] || len(lines) == 0 {
			continue
		}
		ops := make([]patches.LineOp, len(lines))
		for i, l := range lines {
			ops[i] = patches.LineOp{Kind: patches.OpDelete, ID: l.ID}
		}
		out[p] = ops
	}
	return out, nil
}

// writeWorkingTree materializes new to disk, then removes any file present
// in old but absent from new (i.e. deleted between the two points).
func writeWorkingTree(r *repo, old, new patches.Index) error {
	for p, lines := range new {
		full := filepath.Join(r.root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(joinLines(lines)), 0o644); err != nil {
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

func sortedKeys(m map[string][]patches.LineOp) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
