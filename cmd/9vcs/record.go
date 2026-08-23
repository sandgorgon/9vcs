package main

import (
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func cmdRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	message := fs.String("m", "", "patch message")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *message == "" {
		return fmt.Errorf("record: -m MESSAGE is required")
	}

	r, err := findRepo()
	if err != nil {
		return err
	}

	idx, err := patches.LoadIndex(r.indexPath)
	if err != nil {
		return fmt.Errorf("loading index: %w", err)
	}

	paths, err := r.workingFiles()
	if err != nil {
		return fmt.Errorf("walking working tree: %w", err)
	}
	present := make(map[string]bool, len(paths))
	for _, p := range paths {
		present[p] = true
	}

	patch := &patches.Patch{Author: author(), Time: time.Now(), Message: *message}

	for _, p := range paths {
		content, err := os.ReadFile(filepath.Join(r.root, filepath.FromSlash(p)))
		if err != nil {
			return fmt.Errorf("reading %s: %w", p, err)
		}
		newLines := splitLines(string(content))
		ops, newIndex := patches.Diff(idx[p], newLines)
		if len(ops) == 0 {
			continue
		}
		patch.Changes = append(patch.Changes, patches.FileChange{Path: p, Ops: ops})
		idx[p] = newIndex
	}
	// Files that were tracked but no longer exist: delete every remaining line.
	for p, lines := range idx {
		if present[p] || len(lines) == 0 {
			continue
		}
		ops := make([]patches.LineOp, len(lines))
		for i, l := range lines {
			ops[i] = patches.LineOp{Kind: patches.OpDelete, ID: l.ID}
		}
		patch.Changes = append(patch.Changes, patches.FileChange{Path: p, Ops: ops})
		idx[p] = nil
	}

	if len(patch.Changes) == 0 {
		fmt.Println("nothing to record")
		return nil
	}

	head, _, err := r.headHash()
	if err != nil {
		return fmt.Errorf("reading head: %w", err)
	}
	patch.Parent = head

	hash, err := r.store.Put(patch)
	if err != nil {
		return fmt.Errorf("writing patch: %w", err)
	}
	if err := idx.Save(r.indexPath); err != nil {
		return fmt.Errorf("saving index: %w", err)
	}
	if err := r.setHead(hash); err != nil {
		return fmt.Errorf("updating ref: %w", err)
	}

	fmt.Printf("recorded %s: %s\n", hash.String()[:12], *message)
	return nil
}

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
