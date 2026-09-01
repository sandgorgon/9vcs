package main

import (
	"os"
	"path/filepath"
	"testing"
)

func readWorkingFile(t *testing.T, root, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRestoreDiscardsSingleFileEdit(t *testing.T) {
	r := newTestRepo(t)
	writeWorkingFile(t, r, "a.txt", "hello\n")
	recordForTest(t, r, "seed")

	writeWorkingFile(t, r, "a.txt", "edited\n")

	if err := restorePaths(r, []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := readWorkingFile(t, r.Root, "a.txt"); got != "hello\n" {
		t.Errorf("a.txt = %q, want %q", got, "hello\n")
	}
}

func TestRestoreRevertsRenameByNamingBothPaths(t *testing.T) {
	r := newTestRepo(t)
	writeWorkingFile(t, r, "b.txt", "world\n")
	recordForTest(t, r, "seed")

	if err := os.Rename(
		filepath.Join(r.Root, "b.txt"),
		filepath.Join(r.Root, "c.txt"),
	); err != nil {
		t.Fatal(err)
	}

	if err := restorePaths(r, []string{"b.txt", "c.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := readWorkingFile(t, r.Root, "b.txt"); got != "world\n" {
		t.Errorf("b.txt = %q, want %q", got, "world\n")
	}
	if _, err := os.Stat(filepath.Join(r.Root, "c.txt")); !os.IsNotExist(err) {
		t.Errorf("c.txt still exists after restore, want it removed (err=%v)", err)
	}
}

func TestRestoreDeletesUncommittedAdditionWithNoBaseEntry(t *testing.T) {
	r := newTestRepo(t)
	recordForTest(t, r, "seed") // empty history, so root has no tracked paths at all

	writeWorkingFile(t, r, "new.txt", "brand new\n")

	if err := restorePaths(r, []string{"new.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Root, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("new.txt still exists after restore, want it removed (err=%v)", err)
	}
}

func TestRestoreErrorsOnPathWithNoHistoryAndNoChanges(t *testing.T) {
	r := newTestRepo(t)
	writeWorkingFile(t, r, "a.txt", "hello\n")
	recordForTest(t, r, "seed")

	if err := restorePaths(r, []string{"nope.txt"}); err == nil {
		t.Fatal("expected an error for a path with no recorded history and no uncommitted changes")
	}
}

func TestRestoreIsNoopForPathAlreadyMatchingBase(t *testing.T) {
	r := newTestRepo(t)
	writeWorkingFile(t, r, "a.txt", "hello\n")
	recordForTest(t, r, "seed")

	if err := restorePaths(r, []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := readWorkingFile(t, r.Root, "a.txt"); got != "hello\n" {
		t.Errorf("a.txt = %q, want %q", got, "hello\n")
	}
}
