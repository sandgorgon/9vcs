package main

import (
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

// TestSplitJoinRoundTrip checks the bug fixed in this pass: joinLines used
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
		trailing := hasTrailingNewline([]byte(s))
		lines := splitLines(s)
		var pl []patches.Line
		for _, c := range lines {
			pl = append(pl, patches.Line{Content: c})
		}
		got := joinLines(pl, trailing)
		if got != s {
			t.Errorf("round trip failed: splitLines/joinLines(%q) = %q", s, got)
		}
	}
}

func TestIsBinary(t *testing.T) {
	if isBinary([]byte("plain ascii text\nwith newlines\n")) {
		t.Error("isBinary(text) = true, want false")
	}
	if !isBinary([]byte("PNG\x00\x01\x02garbage")) {
		t.Error("isBinary(NUL-containing) = false, want true")
	}
	if isBinary(nil) {
		t.Error("isBinary(nil) = true, want false")
	}
}
