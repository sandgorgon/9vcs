package main

import (
	"testing"

	"github.com/sandgorgon/9vcs/repo"
)

// TestRecordMultiLineDeleteRunStaysCleanAfterRecord is the end-to-end
// regression test for a real bug reported live: recording an edit that
// deletes two or more consecutive lines while also inserting new content
// in the same gap (no concurrent patch involved at all) left the file's
// line graph with a structural fork. `status` then reported the file as
// permanently modified after every future record, and `diff` rendered an
// empty "--- / +++" header with nothing under it — see
// objstore/patches/diff.go's emitGap doc comment for the root cause, and
// objstore/patches/diff_test.go for the algorithm-level regression test.
func TestRecordMultiLineDeleteRunStaysCleanAfterRecord(t *testing.T) {
	r := newTestRepo(t)
	writeWorkingFile(t, r, "dup.txt", "b\nb\nc\n")
	recordForTest(t, r, "init")

	writeWorkingFile(t, r, "dup.txt", "a\nc\nb\n")
	recordForTest(t, r, "test")

	head, _, err := r.HeadHash()
	if err != nil {
		t.Fatal(err)
	}
	base, err := r.Materialize(head)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := repo.ChangedFiles(r, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("ChangedFiles right after record = %v, want empty (working tree should be clean)", repo.SortedPaths(changes))
	}
}
