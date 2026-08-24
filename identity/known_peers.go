package identity

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// KnownPeers is a client-side address -> fingerprint pin: the TOFU
// counterpart to the server-side AuthorizedPeers allowlist. Instead of a
// human curating who may connect in, this records who a peer *turned out
// to be* the first time this install connected out to it, so a later
// connection to the same address presenting a different fingerprint is a
// loud refusal rather than a silent MITM — SSH's known_hosts model (see
// PLAN.md's "known-peers store with TOFU semantics").
type KnownPeers map[string]string // addr -> fingerprint

// KnownPeersPath returns this install's known-peers file path, creating
// its config directory if needed — the file itself need not exist yet.
func KnownPeersPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "known-peers"), nil
}

// LoadKnownPeers parses a known-peers file: one "<address> <fingerprint>"
// pair per line, shaped like authorized-peers/known_hosts — blank lines
// and lines starting with # are ignored. A missing file is not an error;
// it just means no peer has been connected to yet.
func LoadKnownPeers(path string) (KnownPeers, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return KnownPeers{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := KnownPeers{}
	scanner := bufio.NewScanner(f)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: expected \"<address> <fingerprint>\", got %q", path, lineNo, line)
		}
		out[fields[0]] = fields[1]
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RememberPeer records (or updates) addr's fingerprint in the known-peers
// file at path, creating the file and its directory if needed.
// Overwrites any existing entry for addr outright: called either after a
// first-connection TOFU prompt is accepted, or after an explicit
// -peer-fingerprint pin succeeds — both are exactly the moments a human
// has just vouched for the association, including the legitimate
// key-rotation case (re-pin once with -peer-fingerprint, and it's
// remembered again).
func RememberPeer(path, addr, fingerprint string) error {
	kp, err := LoadKnownPeers(path)
	if err != nil {
		return err
	}
	kp[addr] = fingerprint

	addrs := make([]string, 0, len(kp))
	for a := range kp {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)

	var b strings.Builder
	for _, a := range addrs {
		fmt.Fprintf(&b, "%s %s\n", a, kp[a])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
