package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func symlinkTo(t *testing.T, r *repo, path, target string) {
	t.Helper()
	full := filepath.Join(r.root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

func TestChangedFilesDetectsNewSymlink(t *testing.T) {
	r := newTestRepo(t)
	symlinkTo(t, r, "current", "run.sh")

	changes, err := changedFiles(r, patches.Index{})
	if err != nil {
		t.Fatal(err)
	}
	fc, ok := changes["current"]
	if !ok {
		t.Fatal("expected a change for the new symlink")
	}
	if fc.Kind != patches.KindSymlink {
		t.Errorf("Kind = %v, want KindSymlink", fc.Kind)
	}
	if fc.SymlinkTarget != "run.sh" {
		t.Errorf("SymlinkTarget = %q, want %q", fc.SymlinkTarget, "run.sh")
	}
}

func TestChangedFilesSymlinkUnchangedIsNotReported(t *testing.T) {
	r := newTestRepo(t)
	symlinkTo(t, r, "current", "run.sh")
	recordForTest(t, r, "add symlink")

	head, _, err := r.headHash()
	if err != nil {
		t.Fatal(err)
	}
	base, err := r.materialize(head)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := changedFiles(r, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("expected no changes for an unmodified symlink, got %v", changes)
	}
}

// TestChangedFilesExecutableOnlyChangeIsDetected is the regression test
// for the bug this feature would otherwise have shipped with silently:
// changedFiles' unchanged-checks originally compared only content
// (blob hash / line ops + trailing newline), never Executable — so
// chmod +x with no other edit would have been invisible to record/diff/
// status even though the field exists and is correctly encoded.
func TestChangedFilesExecutableOnlyChangeIsDetected(t *testing.T) {
	r := newTestRepo(t)
	full := filepath.Join(r.root, "run.sh")
	if err := os.WriteFile(full, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	recordForTest(t, r, "add script, not executable")

	head, _, err := r.headHash()
	if err != nil {
		t.Fatal(err)
	}
	base, err := r.materialize(head)
	if err != nil {
		t.Fatal(err)
	}
	if base["run.sh"].Executable {
		t.Fatal("test setup: run.sh should not be executable yet")
	}

	if err := os.Chmod(full, 0o755); err != nil {
		t.Fatal(err)
	}
	changes, err := changedFiles(r, base)
	if err != nil {
		t.Fatal(err)
	}
	fc, ok := changes["run.sh"]
	if !ok {
		t.Fatal("chmod +x with no content change must still be reported as a change")
	}
	if !fc.Executable {
		t.Error("Executable = false, want true")
	}
	if len(fc.Ops) != 0 {
		t.Errorf("an exec-bit-only change should carry no line ops, got %v", fc.Ops)
	}
}

func TestWriteWorkingTreeCreatesSymlink(t *testing.T) {
	r := newTestRepo(t)
	new := patches.Index{"current": {Kind: patches.KindSymlink, SymlinkTarget: "run.sh"}}
	if err := writeWorkingTree(r, patches.Index{}, new); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(r.root, "current")
	info, err := os.Lstat(full)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected a symlink on disk")
	}
	got, err := os.Readlink(full)
	if err != nil {
		t.Fatal(err)
	}
	if got != "run.sh" {
		t.Errorf("symlink target = %q, want %q", got, "run.sh")
	}
}

// TestWriteWorkingTreeSetsExecutableBitOnExistingFile is the regression
// test for the other silent-failure risk this feature would otherwise
// have shipped with: os.WriteFile's mode argument only applies when it
// actually creates the file — POSIX open(2) leaves an existing file's
// permission bits untouched — so writing new content to an
// already-existing path (the ordinary case: checkout overwriting what's
// already checked out) would silently fail to actually toggle the
// executable bit without an explicit chmod.
func TestWriteWorkingTreeSetsExecutableBitOnExistingFile(t *testing.T) {
	r := newTestRepo(t)
	full := filepath.Join(r.root, "run.sh")
	if err := os.WriteFile(full, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A real PathState needs a Graph for the text-writing path to work;
	// build one via a genuine patch round trip rather than hand-faking
	// FileGraph internals.
	p := &patches.Patch{Changes: []patches.FileChange{{Path: "run.sh", Kind: patches.KindText, TrailingNewline: true, Executable: true,
		Ops: []patches.LineOp{{Kind: patches.OpInsert, ID: "a", Content: "#!/bin/sh"}}}}}
	h, err := r.store.Put(p)
	if err != nil {
		t.Fatal(err)
	}
	new, err := r.materialize(h)
	if err != nil {
		t.Fatal(err)
	}

	if err := writeWorkingTree(r, patches.Index{}, new); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("run.sh mode = %v, want the executable bit set even though the file already existed", info.Mode())
	}
}

func TestWriteWorkingTreeReplacesSymlinkWithRegularFile(t *testing.T) {
	r := newTestRepo(t)
	full := filepath.Join(r.root, "path")
	if err := os.Symlink("elsewhere", full); err != nil {
		t.Fatal(err)
	}

	p := &patches.Patch{Changes: []patches.FileChange{{Path: "path", Kind: patches.KindText, TrailingNewline: true,
		Ops: []patches.LineOp{{Kind: patches.OpInsert, ID: "a", Content: "now a real file"}}}}}
	h, err := r.store.Put(p)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := r.materialize(h)
	if err != nil {
		t.Fatal(err)
	}

	if err := writeWorkingTree(r, patches.Index{}, idx); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(full)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("path should no longer be a symlink")
	}
	got, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "now a real file\n" {
		t.Errorf("content = %q, want the new file's content, not a write through the stale symlink", got)
	}
}

// TestComputeMergeThreeWaySymlinkConflict mirrors
// TestComputeMergeThreeWayBinaryConflict's shape exactly — same
// keep-roots[0], flag-the-rest policy, generalized to symlink targets.
func TestComputeMergeThreeWaySymlinkConflict(t *testing.T) {
	r := newTestRepo(t)

	base, idx := recordTestPatch(t, r, nil, "keep.txt", []string{"unrelated"}, patches.Index{})
	_ = idx

	a, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "a", Changes: []patches.FileChange{{Path: "current", Kind: patches.KindSymlink, SymlinkTarget: "v-a"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "b", Changes: []patches.FileChange{{Path: "current", Kind: patches.KindSymlink, SymlinkTarget: "v-b"}}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.store.Put(&patches.Patch{Dependencies: []patches.Hash{base}, Message: "c", Changes: []patches.FileChange{{Path: "current", Kind: patches.KindSymlink, SymlinkTarget: "v-c"}}})
	if err != nil {
		t.Fatal(err)
	}

	merged, conflicts, err := computeMerge(r, a, b, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 || conflicts[0].Kind != "symlink" || conflicts[0].Path != "current" {
		t.Fatalf("expected one symlink conflict on current, got %v", conflicts)
	}
	wantOthers := []string{"v-b", "v-c"}
	if len(conflicts[0].OtherTargets) != 2 || conflicts[0].OtherTargets[0] != wantOthers[0] || conflicts[0].OtherTargets[1] != wantOthers[1] {
		t.Errorf("OtherTargets = %v, want %v", conflicts[0].OtherTargets, wantOthers)
	}
	if merged["current"].SymlinkTarget != "v-a" {
		t.Errorf("expected roots[0]'s (a's) target kept, got %q", merged["current"].SymlinkTarget)
	}
}

func TestRenameCandidateSymlinkExactMatch(t *testing.T) {
	oldSt := patches.PathState{Kind: patches.KindSymlink, SymlinkTarget: "same-target"}
	newFc := patches.FileChange{Path: "new-name", Kind: patches.KindSymlink, SymlinkTarget: "same-target"}
	score, pair, ok := renameCandidate("old-name", oldSt, "new-name", newFc)
	if !ok || score != 1.0 || pair.modified {
		t.Fatalf("renameCandidate = score %v pair %+v ok %v, want 1.0/unmodified/true", score, pair, ok)
	}
}

func TestRenameCandidateSymlinkRetargetedIsNotDetectedAsRename(t *testing.T) {
	oldSt := patches.PathState{Kind: patches.KindSymlink, SymlinkTarget: "old-target"}
	newFc := patches.FileChange{Path: "new-name", Kind: patches.KindSymlink, SymlinkTarget: "new-target"}
	if _, _, ok := renameCandidate("old-name", oldSt, "new-name", newFc); ok {
		t.Error("a symlink renamed and retargeted in the same change should not be detected — same policy as KindBlob")
	}
}
