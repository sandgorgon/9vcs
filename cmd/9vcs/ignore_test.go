package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadIgnoreMissingFileIsNoPatterns(t *testing.T) {
	m, err := loadIgnore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 0 {
		t.Errorf("loadIgnore on a repo with no %s = %v, want empty", ignoreFileName, m)
	}
	if m.matches("anything.go") {
		t.Error("an empty matcher should never match")
	}
}

func writeIgnoreFile(t *testing.T, root, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ignoreFileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadIgnoreSkipsBlankLinesAndComments(t *testing.T) {
	root := t.TempDir()
	writeIgnoreFile(t, root, "\n# a comment\n   \n*.log\n# another\n")
	m, err := loadIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 1 {
		t.Fatalf("got %d patterns, want 1 (comments/blanks skipped): %v", len(m), m)
	}
}

func TestIgnoreBasenameGlobMatchesAtAnyDepth(t *testing.T) {
	root := t.TempDir()
	writeIgnoreFile(t, root, "*.log\n")
	m, err := loadIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"debug.log", "nested/dir/debug.log"} {
		if !m.matches(p) {
			t.Errorf("matches(%q) = false, want true", p)
		}
	}
	if m.matches("debug.txt") {
		t.Error("matches(debug.txt) = true, want false")
	}
}

func TestIgnoreAnchoredPatternMatchesOnlyAtRoot(t *testing.T) {
	root := t.TempDir()
	writeIgnoreFile(t, root, "/build\n")
	m, err := loadIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.matches("build") {
		t.Error("matches(build) = false, want true (root-level match)")
	}
	if m.matches("sub/build") {
		t.Error("matches(sub/build) = true, want false (anchored to root)")
	}
}

func TestIgnoreDirOnlyPatternSkipsWholeSubtreeNotFilesOfTheSameName(t *testing.T) {
	root := t.TempDir()
	writeIgnoreFile(t, root, "node_modules/\n")
	m, err := loadIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.matches("node_modules/left-pad/index.js") {
		t.Error("a file under node_modules/ should be ignored")
	}
	if !m.matches("vendor/node_modules/index.js") {
		t.Error("node_modules/ (unanchored) should match at any depth")
	}
	// A file literally named "node_modules" (no trailing content) is not a
	// directory, so a dirOnly pattern must not match it as the final path
	// segment.
	if m.matches("node_modules") {
		t.Error("a plain file named node_modules should not match a directory-only pattern")
	}
}

func TestIgnoreSlashAnchoredPatternWithGlob(t *testing.T) {
	root := t.TempDir()
	writeIgnoreFile(t, root, "src/gen/*.go\n")
	m, err := loadIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.matches("src/gen/types.go") {
		t.Error("matches(src/gen/types.go) = false, want true")
	}
	if m.matches("other/gen/types.go") {
		t.Error("matches(other/gen/types.go) = true, want false (anchored)")
	}
}

// TestIgnorePatternWithoutTrailingSlashStillCoversDirectoryContents pins a
// real bug caught by a CLI smoke test, not a unit test: "/build" (no
// trailing slash) matched only the literal path "build", never
// "build/out.bin" beneath it — so a file under an ignored directory
// leaked into changedFiles' output. Real gitignore's rule is that
// matching a directory always prunes its whole subtree regardless of
// whether the pattern happened to end in "/"; only an explicit trailing
// "/" additionally *requires* the match to land on a directory rather
// than a same-named file (see the dirOnly test above).
func TestIgnorePatternWithoutTrailingSlashStillCoversDirectoryContents(t *testing.T) {
	root := t.TempDir()
	writeIgnoreFile(t, root, "/build\n")
	m, err := loadIgnore(root)
	if err != nil {
		t.Fatal(err)
	}
	if !m.matches("build/out.bin") {
		t.Error("matches(build/out.bin) = false, want true — /build must prune everything beneath it")
	}
	if !m.matches("build/nested/deep.o") {
		t.Error("matches(build/nested/deep.o) = false, want true — pruning must apply at any depth under the matched directory")
	}
}

func TestLoadIgnoreBadPatternIsALoudError(t *testing.T) {
	root := t.TempDir()
	writeIgnoreFile(t, root, "[unterminated\n")
	if _, err := loadIgnore(root); err == nil {
		t.Error("expected a malformed pattern to be reported at load time, got nil")
	}
}
