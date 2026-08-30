package repo

import (
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// TestSplitJoinRoundTrip checks the bug fixed in this pass: JoinLines used
// to always append a trailing newline, corrupting any file — text or
// binary — that didn't originally end in one.
func TestSplitJoinRoundTrip(t *testing.T) {
	cases := []string{
		"",
		"a\n",
		"a",
		"a\nb\n",
		"a\nb",
		"\n",
		"line one\nline two\nline three\n",
		"no newline at all",
	}
	for _, s := range cases {
		trailing := HasTrailingNewline([]byte(s))
		lines := SplitLines(s)
		var pl []patches.Line
		for _, c := range lines {
			pl = append(pl, patches.Line{Content: c})
		}
		got := JoinLines(pl, trailing)
		if got != s {
			t.Errorf("round trip failed: SplitLines/JoinLines(%q) = %q", s, got)
		}
	}
}

func TestIsBinary(t *testing.T) {
	if IsBinary([]byte("plain ascii text\nwith newlines\n")) {
		t.Error("IsBinary(text) = true, want false")
	}
	if !IsBinary([]byte("PNG\x00\x01\x02garbage")) {
		t.Error("IsBinary(NUL-containing) = false, want true")
	}
	if IsBinary(nil) {
		t.Error("IsBinary(nil) = true, want false")
	}
}
