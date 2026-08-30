package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sandgorgon/9vcs/repo"
)

// writeIgnoreFile writes a .9vcsignore at root — used by tests elsewhere
// in this package that exercise repo.ChangedFiles' integration with
// ignore patterns (status_test.go, changedfiles_ignore_test.go). Unit
// tests for the ignore-matching logic itself now live in
// repo/ignore_test.go, alongside the (unexported) types they exercise.
func writeIgnoreFile(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, repo.IgnoreFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
