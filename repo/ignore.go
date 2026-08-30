package repo

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// IgnoreFileName lives at the repo root, alongside tracked files — unlike
// authorized-peers/known-peers/config (host-specific, under .9vcs, never
// recorded), this one is meant to be recorded and shared with the team,
// same as .gitignore. See PLAN.md decision #2's "Ignore patterns —
// concrete scope" for the full design and its deliberate scope cuts
// (no "!" negation, no "**", a single top-level file only).
const IgnoreFileName = ".9vcsignore"

// ignorePattern is one parsed line from .9vcsignore.
type ignorePattern struct {
	globSegs []string // pat.glob split on "/" — path.Match doesn't cross "/", so matching is done per aligned segment window
	anchored bool     // matched only starting at path segment 0; unanchored patterns slide the window to any starting depth
	dirOnly  bool     // trailing "/" in the source line: the match must land on a real ancestor directory, never the final filename
}

// LoadIgnore reads .9vcsignore at root. A missing file means no patterns —
// the same "missing file is not an error" convention every other flat
// text file in this codebase already uses (authorized-peers, known-peers,
// 9vcs config).
func LoadIgnore(root string) (ignoreMatcher, error) {
	f, err := os.Open(filepath.Join(root, IgnoreFileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out ignoreMatcher
	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pat, err := parseIgnorePattern(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", IgnoreFileName, lineNo, err)
		}
		out = append(out, pat)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// parseIgnorePattern validates the glob eagerly (via a throwaway
// path.Match call) so a typo'd pattern is a loud error at load time,
// rather than a silently-never-matching one at scan time.
func parseIgnorePattern(line string) (ignorePattern, error) {
	dirOnly := strings.HasSuffix(line, "/")
	if dirOnly {
		line = strings.TrimSuffix(line, "/")
	}
	anchored := strings.HasPrefix(line, "/")
	line = strings.TrimPrefix(line, "/")
	anchored = anchored || strings.Contains(line, "/")

	if _, err := path.Match(line, ""); err != nil {
		return ignorePattern{}, fmt.Errorf("bad pattern %q: %w", line, err)
	}
	return ignorePattern{globSegs: strings.Split(line, "/"), anchored: anchored, dirOnly: dirOnly}, nil
}

// matches reports whether p — a repo-relative, slash-separated file path
// (WorkingFiles never returns directories) — is covered by pat.
//
// Matching any directory component along p, not just the final filename,
// excludes everything beneath it — the same rule real gitignore uses: a
// pattern matching a directory always prunes its whole subtree, trailing
// "/" or not. The trailing "/" only narrows what counts as a match at
// all: it must land on a genuine ancestor directory, so a bare file that
// happens to share the pattern's name isn't caught by a directory-only
// rule (see TestIgnoreDirOnlyPatternSkipsWholeSubtreeNotFilesOfTheSameName).
func (pat ignorePattern) matches(p string) bool {
	segments := strings.Split(p, "/")
	g := len(pat.globSegs)

	maxStart := len(segments) - g
	if pat.anchored {
		maxStart = 0 // only the window starting at the root is considered
	}
	glob := strings.Join(pat.globSegs, "/")
	for start := 0; start <= maxStart; start++ {
		end := start + g
		if pat.dirOnly && end == len(segments) {
			continue // would land on the final segment (the file itself), never a directory
		}
		// The error return is a malformed pattern, already rejected by
		// parseIgnorePattern at load time — it can't fire here.
		if ok, _ := path.Match(glob, strings.Join(segments[start:end], "/")); ok {
			return true
		}
	}
	return false
}

// ignoreMatcher is a parsed .9vcsignore: every pattern is a plain
// inclusion test, ORed together — there's no "!" negation to reorder
// around (see the package doc comment on IgnoreFileName).
type ignoreMatcher []ignorePattern

func (m ignoreMatcher) matches(p string) bool {
	for _, pat := range m {
		if pat.matches(p) {
			return true
		}
	}
	return false
}
