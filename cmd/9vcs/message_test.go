package main

import (
	"flag"
	"testing"
)

func parseMessage(t *testing.T, args ...string) string {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var m messageFlag
	fs.Var(&m, "m", "")
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	return m.Message()
}

func TestMessageFlagSingle(t *testing.T) {
	if got := parseMessage(t, "-m", "fix parser"); got != "fix parser" {
		t.Errorf("got %q", got)
	}
}

func TestMessageFlagRepeatedJoinsParagraphs(t *testing.T) {
	got := parseMessage(t, "-m", "subject", "-m", "body one", "-m", "body two")
	want := "subject\n\nbody one\n\nbody two"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMessageFlagKeepsEmbeddedNewlines(t *testing.T) {
	got := parseMessage(t, "-m", "subject\n\nline a\nline b")
	want := "subject\n\nline a\nline b"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMessageFlagUnset(t *testing.T) {
	if got := parseMessage(t); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestCleanMessage(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"whitespace only", " \n\t\n  ", ""},
		{"trailing spaces per line", "a  \nb\t", "a\nb"},
		{"leading and trailing blank lines", "\n\na\nb\n\n\n", "a\nb"},
		{"crlf", "a\r\n\r\nb\r\n", "a\n\nb"},
		{"interior blank lines kept", "a\n\n\nb", "a\n\n\nb"},
		{"leading indentation kept", "  a\n    b", "  a\n    b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanMessage(tt.in); got != tt.want {
				t.Errorf("cleanMessage(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestMessageSubject(t *testing.T) {
	if got := messageSubject("one\n\ntwo"); got != "one" {
		t.Errorf("got %q", got)
	}
	if got := messageSubject("only"); got != "only" {
		t.Errorf("got %q", got)
	}
	if got := messageSubject(""); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestIndentMessage(t *testing.T) {
	got := indentMessage("subject\n\nbody")
	want := "    subject\n\n    body"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
