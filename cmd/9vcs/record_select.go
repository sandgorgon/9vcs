package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

// pathIDsFlag collects repeated "-lines PATH:ID[,ID...]" flags into a
// repo.Selection-shaped map.
type pathIDsFlag struct{ m map[string]map[string]bool }

func (f *pathIDsFlag) String() string { return "" }

func (f *pathIDsFlag) Set(s string) error {
	path, idsPart, ok := strings.Cut(s, ":")
	if !ok || path == "" || idsPart == "" {
		return fmt.Errorf("expected PATH:ID[,ID...], got %q", s)
	}
	if f.m == nil {
		f.m = map[string]map[string]bool{}
	}
	ids := f.m[path]
	if ids == nil {
		ids = map[string]bool{}
		f.m[path] = ids
	}
	for _, id := range strings.Split(idsPart, ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids[id] = true
		}
	}
	return nil
}

// pathsFlag collects repeated "-files PATH[,PATH...]" flags.
type pathsFlag struct{ m map[string]bool }

func (f *pathsFlag) String() string { return "" }

func (f *pathsFlag) Set(s string) error {
	if f.m == nil {
		f.m = map[string]bool{}
	}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			f.m[p] = true
		}
	}
	return nil
}

// lineContentByID resolves a KindText FileChange's line ids back to
// display text: an insert op already carries its own Content, but a
// delete op only names the id of a line already alive in base, so that
// side needs an explicit lookup.
func lineContentByID(base patches.Index, path string) map[string]string {
	out := map[string]string{}
	prior, ok := base[path]
	if !ok || prior.Kind != patches.KindText || prior.Graph == nil {
		return out
	}
	lines, _ := patches.Linearize(prior.Graph)
	for _, l := range lines {
		out[l.ID] = l.Content
	}
	return out
}

// interactiveSelect prompts per file, then per line-level op within a
// text file, darcs-record-style: y keeps it for this patch, n leaves it
// pending, q stops asking (whatever was already answered "y" still
// counts). Whether selective record is even allowed right now (e.g. not
// mid-merge) is the caller's concern (see cmdRecord), not this
// function's.
func interactiveSelect(changes map[string]patches.FileChange, base patches.Index, in io.Reader, out io.Writer) (map[string]patches.FileChange, error) {
	reader := bufio.NewReader(in)
	sel := repo.Selection{Files: map[string]bool{}, Lines: map[string]map[string]bool{}}

paths:
	for _, path := range repo.SortedPaths(changes) {
		fc := changes[path]
		if fc.Kind != patches.KindText {
			ans, err := promptYNQ(reader, out, fmt.Sprintf("%s %s?", statusLabel(fc, base), path))
			if err != nil {
				return nil, err
			}
			switch ans {
			case 'y':
				sel.Files[path] = true
			case 'q':
				break paths
			}
			continue
		}

		content := lineContentByID(base, path)
		for _, op := range fc.Ops {
			var line string
			switch op.Kind {
			case patches.OpInsert:
				line = "+ " + op.Content
			case patches.OpDelete:
				line = "- " + content[op.ID]
			default:
				continue // fork-resolution ops (OpSever/OpLink) aren't line-selectable
			}
			ans, err := promptYNQ(reader, out, fmt.Sprintf("%s\n%s\nrecord this change?", path, line))
			if err != nil {
				return nil, err
			}
			switch ans {
			case 'y':
				if sel.Lines[path] == nil {
					sel.Lines[path] = map[string]bool{}
				}
				sel.Lines[path][op.ID] = true
			case 'q':
				break paths
			}
		}
	}

	if len(sel.Files) == 0 && len(sel.Lines) == 0 {
		return nil, fmt.Errorf("no changes selected")
	}
	return sel.Apply(changes)
}

// promptYNQ prints prompt and reads a single y/n/q answer, reprompting on
// anything else (including a blank line).
func promptYNQ(reader *bufio.Reader, out io.Writer, prompt string) (byte, error) {
	for {
		fmt.Fprintf(out, "%s [y,n,q] ", prompt)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return 0, err
		}
		switch strings.TrimSpace(line) {
		case "y", "Y":
			return 'y', nil
		case "n", "N":
			return 'n', nil
		case "q", "Q":
			return 'q', nil
		}
		fmt.Fprintln(out, `please answer "y", "n", or "q"`)
	}
}
