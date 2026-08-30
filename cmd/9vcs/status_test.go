package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

func TestStatusLabel(t *testing.T) {
	base := patches.Index{
		"tracked.txt": {Kind: patches.KindText},
	}
	cases := []struct {
		name string
		fc   patches.FileChange
		want string
	}{
		{"delete", patches.FileChange{Path: "tracked.txt", Kind: patches.KindDelete}, "D"},
		{"new path", patches.FileChange{Path: "new.txt", Kind: patches.KindText}, "A"},
		{"new blob", patches.FileChange{Path: "new.bin", Kind: patches.KindBlob}, "A"},
		{"modified existing", patches.FileChange{Path: "tracked.txt", Kind: patches.KindText}, "M"},
		{
			"unresolved conflict marker",
			patches.FileChange{
				Path: "tracked.txt",
				Kind: patches.KindText,
				Ops: []patches.LineOp{
					// The literal marker text patches.IsMarker recognizes
					// (see objstore/patches/linearize.go's conflictOpen) —
					// unexported there, so reproduced here rather than
					// exporting a constant just for this test.
					{Kind: patches.OpInsert, Content: "<<<<<<< 9vcs conflict"},
				},
			},
			"U",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := statusLabel(c.fc, base)
			if got != c.want {
				t.Errorf("statusLabel(%+v) = %q, want %q", c.fc, got, c.want)
			}
		})
	}
}

// TestStatusReportsAddedModifiedDeletedAndSkipsIgnored is an integration
// test over changedFiles + statusLabel together, the same pairing
// cmdStatus itself uses.
func TestStatusReportsAddedModifiedDeletedAndSkipsIgnored(t *testing.T) {
	r := newTestRepo(t)
	writeWorkingFile(t, r, "keep.txt", "original")
	writeWorkingFile(t, r, "gone.txt", "will be deleted")
	recordForTest(t, r, "seed")

	writeWorkingFile(t, r, "keep.txt", "edited")
	writeWorkingFile(t, r, "new.txt", "brand new")
	writeIgnoreFile(t, r.Root, "*.log\n")
	writeWorkingFile(t, r, "noise.log", "should never show up")
	if err := os.Remove(filepath.Join(r.Root, "gone.txt")); err != nil {
		t.Fatal(err)
	}

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

	want := map[string]string{
		"keep.txt":          "M",
		"new.txt":           "A",
		"gone.txt":          "D",
		repo.IgnoreFileName: "A", // .9vcsignore itself is newly recorded here, and correctly not exempt from its own rules
	}
	if len(changes) != len(want) {
		t.Fatalf("ChangedFiles = %v, want exactly %v (noise.log must be excluded)", repo.SortedPaths(changes), want)
	}
	for p, wantLabel := range want {
		fc, ok := changes[p]
		if !ok {
			t.Errorf("missing expected change for %s", p)
			continue
		}
		if got := statusLabel(fc, base); got != wantLabel {
			t.Errorf("statusLabel(%s) = %q, want %q", p, got, wantLabel)
		}
	}
}
