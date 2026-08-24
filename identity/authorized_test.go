package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAuthorizedPeers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized-peers")
	content := "# comment\n\nabc123 read\ndef456 write\n789abc propose\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	peers, err := LoadAuthorizedPeers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 3 {
		t.Fatalf("got %d peers, want 3: %v", len(peers), peers)
	}

	cases := []struct {
		fp   string
		need Permission
		want bool
	}{
		{"abc123", PermRead, true},
		{"abc123", PermWrite, false},
		{"def456", PermWrite, true},
		{"def456", PermRead, true}, // write implies read
		{"789abc", PermPropose, true},
		{"789abc", PermWrite, false},
		{"unknown", PermRead, false},
	}
	for _, c := range cases {
		if got := peers.Allows(c.fp, c.need); got != c.want {
			t.Errorf("Allows(%q, %v) = %v, want %v", c.fp, c.need, got, c.want)
		}
	}
}

func TestLoadAuthorizedPeersMissingFile(t *testing.T) {
	peers, err := LoadAuthorizedPeers(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 0 {
		t.Fatalf("expected empty allowlist, got %v", peers)
	}
}

func TestLoadAuthorizedPeersMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized-peers")
	if err := os.WriteFile(path, []byte("onefield\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAuthorizedPeers(path); err == nil {
		t.Fatal("expected an error for a malformed line, got nil")
	}
}
