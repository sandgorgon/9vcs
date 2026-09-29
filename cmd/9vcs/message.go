package main

import "strings"

// messageFlag is a repeatable -m flag: like git, each occurrence becomes
// its own paragraph, joined by a blank line. A single -m whose value
// already contains newlines is kept as-is, so `-m "subject\n\nbody"` (or
// $'...' / a quoted multi-line shell string) works the same as two -m's.
type messageFlag struct {
	parts []string
}

func (m *messageFlag) String() string {
	if m == nil {
		return ""
	}
	return m.Message()
}

func (m *messageFlag) Set(s string) error {
	m.parts = append(m.parts, s)
	return nil
}

// Message returns the cleaned-up message: paragraphs joined by a blank
// line, CRLF normalized, trailing whitespace stripped from every line,
// and leading/trailing blank lines dropped. Empty if nothing but
// whitespace was given.
func (m *messageFlag) Message() string {
	return cleanMessage(strings.Join(m.parts, "\n\n"))
}

func cleanMessage(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// messageSubject returns the first line of msg, for one-line summaries.
func messageSubject(msg string) string {
	subject, _, _ := strings.Cut(msg, "\n")
	return subject
}

// indentMessage prints msg indented by four spaces per line, git-log
// style; blank lines stay empty rather than trailing whitespace.
func indentMessage(msg string) string {
	lines := strings.Split(msg, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = "    " + l
		}
	}
	return strings.Join(lines, "\n")
}
