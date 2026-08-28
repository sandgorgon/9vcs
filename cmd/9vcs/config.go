package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// configKeys are the only keys `9vcs config` currently understands — not
// a general config system yet, just the two fields author() needs. The
// flat file format below doesn't need to change when this list grows.
var configKeys = map[string]bool{
	"user.name":  true,
	"user.email": true,
}

// repoConfigPath is this repo's local config file, checked first by
// resolveConfigValue — same directory as refs/HEAD/authorized-peers.
func repoConfigPath(r *repo) string { return filepath.Join(r.dir, "config") }

// globalConfigPath is this install's user-wide config file, the fallback
// for any key not set in a repo's local config.
func globalConfigPath() (string, error) {
	dir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config"), nil
}

// userConfigDir is 9vcs's own ~/.config/9vcs directory — deliberately
// independent of auth.ConfigDir() (~/.config/9), which moved out from
// under this path when the identity/known-peers material was extracted
// into github.com/sandgorgon/9auth. This file (user.name/user.email
// preferences) isn't key material shared across 9-family programs and
// has no migration story, so it stays where existing installs already
// have it.
func userConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "9vcs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// loadConfigFile parses a flat "key = value" file: one entry per line,
// split on the first '=' only (a value may itself legitimately contain
// one), both sides trimmed. Blank lines and lines starting with '#' are
// ignored. A missing file is not an error — same convention
// auth.LoadKnownPeers uses — it just means nothing is configured
// there yet.
func loadConfigFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected \"<key> = <value>\", got %q", path, lineNo, line)
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// saveConfigFile writes m back as sorted "key = value" lines, creating
// path's directory if needed — same overwrite-outright, sorted-for-
// stable-diffs shape auth.RememberPeer already uses for known-peers.
func saveConfigFile(path string, m map[string]string) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s\n", k, m[k])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// resolveConfigValue resolves key by checking repoPath first, then
// globalPath — per-key cascading, like `git config`: a repo can override
// just one key locally while inheriting everything else set globally.
// Either path may be empty or not exist yet; that's not an error, just
// "unset there".
func resolveConfigValue(repoPath, globalPath, key string) (string, error) {
	if repoPath != "" {
		m, err := loadConfigFile(repoPath)
		if err != nil {
			return "", err
		}
		if v, ok := m[key]; ok {
			return v, nil
		}
	}
	if globalPath != "" {
		m, err := loadConfigFile(globalPath)
		if err != nil {
			return "", err
		}
		if v, ok := m[key]; ok {
			return v, nil
		}
	}
	return "", nil
}

// resolvedAuthorField is resolveConfigValue against this repo's real
// local and global config paths — what author() calls to build the
// Author string, and what `9vcs config <key>` (without -global) prints.
func resolvedAuthorField(r *repo, key string) (string, error) {
	global, err := globalConfigPath()
	if err != nil {
		return "", err
	}
	var repoPath string
	if r != nil {
		repoPath = repoConfigPath(r)
	}
	return resolveConfigValue(repoPath, global, key)
}

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ExitOnError)
	global := fs.Bool("global", false, "read/write the user-wide config instead of this repo's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 1 || len(rest) > 2 {
		return fmt.Errorf("config: usage: 9vcs config [-global] <key> [<value>]")
	}
	key := rest[0]
	if !configKeys[key] {
		return fmt.Errorf("config: unknown key %q (supported: user.name, user.email)", key)
	}

	if len(rest) == 2 {
		return setConfigValue(*global, key, rest[1])
	}
	return getConfigValue(*global, key)
}

// setConfigValue writes key=value to the repo-local config by default,
// or the user-wide one with global — the latter needs no repo at all,
// same as `9vcs identity show` working outside a repo.
func setConfigValue(global bool, key, value string) error {
	var path string
	var err error
	if global {
		path, err = globalConfigPath()
	} else {
		var r *repo
		r, err = findRepo()
		if err == nil {
			path = repoConfigPath(r)
		}
	}
	if err != nil {
		return err
	}
	m, err := loadConfigFile(path)
	if err != nil {
		return err
	}
	m[key] = value
	return saveConfigFile(path, m)
}

// getConfigValue prints key's resolved value: with global, only the
// user-wide file is consulted; otherwise the full repo-local-then-global
// cascade resolveConfigValue implements.
func getConfigValue(global bool, key string) error {
	var (
		value string
		err   error
	)
	if global {
		var path string
		path, err = globalConfigPath()
		if err == nil {
			var m map[string]string
			m, err = loadConfigFile(path)
			if err == nil {
				value = m[key]
			}
		}
	} else {
		var r *repo
		r, err = findRepo()
		if err == nil {
			value, err = resolvedAuthorField(r, key)
		}
	}
	if err != nil {
		return err
	}
	if value == "" {
		return fmt.Errorf("config: %q is not set", key)
	}
	fmt.Println(value)
	return nil
}
