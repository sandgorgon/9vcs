package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sandgorgon/9auth"
)

func TestVerifyPeerPinnedMatchRemembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known-peers")
	known := auth.KnownPeers{}

	if err := verifyPeer(path, known, "peer:1234", "abc123", "abc123", strings.NewReader("")); err != nil {
		t.Fatalf("verifyPeer: %v", err)
	}

	got, err := auth.LoadKnownPeers(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["peer:1234"] != "abc123" {
		t.Errorf("known-peers not updated after a successful pin: got %v", got)
	}
}

func TestVerifyPeerPinnedMismatchRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known-peers")
	known := auth.KnownPeers{}

	err := verifyPeer(path, known, "peer:1234", "expected", "actual", strings.NewReader(""))
	if err == nil {
		t.Fatal("expected an error for a pin mismatch, got nil")
	}

	got, _ := auth.LoadKnownPeers(path)
	if len(got) != 0 {
		t.Errorf("known-peers should be untouched after a rejected pin, got %v", got)
	}
}

func TestVerifyPeerKnownMatchSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known-peers")
	known := auth.KnownPeers{"peer:1234": "abc123"}

	if err := verifyPeer(path, known, "peer:1234", "", "abc123", strings.NewReader("")); err != nil {
		t.Fatalf("verifyPeer: %v", err)
	}
}

func TestVerifyPeerKnownMismatchRefusesWithoutPrompting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known-peers")
	known := auth.KnownPeers{"peer:1234": "old-fp"}

	// A reader that would error if actually consulted — a fingerprint
	// change against an already-known peer must never fall through to
	// the first-connect prompt.
	err := verifyPeer(path, known, "peer:1234", "", "new-fp", errReader{})
	if err == nil {
		t.Fatal("expected an error for a known-peer fingerprint mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "CHANGED") {
		t.Errorf("error doesn't call out the fingerprint change: %v", err)
	}
}

func TestVerifyPeerFirstConnectAcceptRemembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known-peers")
	known := auth.KnownPeers{}

	if err := verifyPeer(path, known, "peer:1234", "", "abc123", strings.NewReader("y\n")); err != nil {
		t.Fatalf("verifyPeer: %v", err)
	}

	got, err := auth.LoadKnownPeers(path)
	if err != nil {
		t.Fatal(err)
	}
	if got["peer:1234"] != "abc123" {
		t.Errorf("known-peers not recorded after an accepted first-connect prompt: got %v", got)
	}
}

func TestVerifyPeerFirstConnectDeclineRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known-peers")
	known := auth.KnownPeers{}

	err := verifyPeer(path, known, "peer:1234", "", "abc123", strings.NewReader("n\n"))
	if err == nil {
		t.Fatal("expected an error for a declined first-connect prompt, got nil")
	}

	got, _ := auth.LoadKnownPeers(path)
	if len(got) != 0 {
		t.Errorf("known-peers should be untouched after a declined prompt, got %v", got)
	}
}

func TestPromptTrustPeerAcceptsYVariants(t *testing.T) {
	for _, in := range []string{"y\n", "Y\n", "yes\n", "YES\n", "  y  \n"} {
		trust, err := promptTrustPeer(strings.NewReader(in), "peer:1234", "abc123")
		if err != nil {
			t.Fatalf("input %q: %v", in, err)
		}
		if !trust {
			t.Errorf("input %q: got trust=false, want true", in)
		}
	}
}

func TestPromptTrustPeerRejectsAnythingElse(t *testing.T) {
	for _, in := range []string{"n\n", "no\n", "\n", ""} {
		trust, err := promptTrustPeer(strings.NewReader(in), "peer:1234", "abc123")
		if err != nil {
			t.Fatalf("input %q: %v", in, err)
		}
		if trust {
			t.Errorf("input %q: got trust=true, want false", in)
		}
	}
}

// errReader always fails on Read — used to assert a code path never
// reaches the point of consulting input at all.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errAlwaysFails }

var errAlwaysFails = errFailure("errReader: should not have been read")

type errFailure string

func (e errFailure) Error() string { return string(e) }
