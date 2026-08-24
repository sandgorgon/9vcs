package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadKnownPeers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known-peers")
	content := "# comment\n\n127.0.0.1:4921 abc123\nexample.com:4921 def456\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	kp, err := LoadKnownPeers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(kp) != 2 {
		t.Fatalf("got %d entries, want 2: %v", len(kp), kp)
	}
	if kp["127.0.0.1:4921"] != "abc123" {
		t.Errorf("kp[127.0.0.1:4921] = %q, want abc123", kp["127.0.0.1:4921"])
	}
	if kp["example.com:4921"] != "def456" {
		t.Errorf("kp[example.com:4921] = %q, want def456", kp["example.com:4921"])
	}
}

func TestLoadKnownPeersMissingFile(t *testing.T) {
	kp, err := LoadKnownPeers(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(kp) != 0 {
		t.Fatalf("expected empty store, got %v", kp)
	}
}

func TestLoadKnownPeersMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known-peers")
	if err := os.WriteFile(path, []byte("onefield\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKnownPeers(path); err == nil {
		t.Fatal("expected an error for a malformed line, got nil")
	}
}

func TestRememberPeerCreatesAndAppends(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "known-peers")

	if err := RememberPeer(path, "host-a:1234", "aaaa"); err != nil {
		t.Fatal(err)
	}
	if err := RememberPeer(path, "host-b:1234", "bbbb"); err != nil {
		t.Fatal(err)
	}

	kp, err := LoadKnownPeers(path)
	if err != nil {
		t.Fatal(err)
	}
	if kp["host-a:1234"] != "aaaa" || kp["host-b:1234"] != "bbbb" {
		t.Errorf("got %v, want both entries present", kp)
	}
}

func TestRememberPeerOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "known-peers")

	if err := RememberPeer(path, "host-a:1234", "old-fp"); err != nil {
		t.Fatal(err)
	}
	if err := RememberPeer(path, "host-a:1234", "new-fp"); err != nil {
		t.Fatal(err)
	}

	kp, err := LoadKnownPeers(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(kp) != 1 {
		t.Fatalf("got %d entries, want 1 (overwritten, not duplicated): %v", len(kp), kp)
	}
	if kp["host-a:1234"] != "new-fp" {
		t.Errorf("kp[host-a:1234] = %q, want new-fp", kp["host-a:1234"])
	}
}
