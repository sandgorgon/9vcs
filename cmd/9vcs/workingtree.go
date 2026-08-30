package main

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"os/user"

	"github.com/sandgorgon/9auth"
	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

// author resolves this record's Author string: "Name <email>" if both
// user.name and user.email are configured (see resolvedAuthorField for
// the repo-local-then-global precedence), "Name" alone if only
// user.name is, or the OS username if neither is configured — unchanged
// fallback, so a fresh, unconfigured install behaves exactly as before.
// A malformed config file (hand-edited, since `9vcs config` itself never
// writes one) is a real error here rather than a silent fallback — it's
// a user mistake worth surfacing, not an incidental environment failure.
func author(r *repo.Repo) (string, error) {
	name, err := resolvedAuthorField(r, "user.name")
	if err != nil {
		return "", fmt.Errorf("resolving user.name: %w", err)
	}
	email, err := resolvedAuthorField(r, "user.email")
	if err != nil {
		return "", fmt.Errorf("resolving user.email: %w", err)
	}
	return formatAuthor(name, email), nil
}

// formatAuthor is author's pure formatting step, split out so it's
// testable without touching any config file or the real OS/global
// config directory: "Name <email>" if both are set, "Name" alone if
// only name is, otherwise the OS username fallback.
func formatAuthor(name, email string) string {
	switch {
	case name != "" && email != "":
		return fmt.Sprintf("%s <%s>", name, email)
	case name != "":
		return name
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

// signPatch signs patch with this install's identity, in place, before
// record stores it: sets AuthorFingerprint to the public key and
// AuthorSignature over patch.SignablePayload(). auth.Load() failing
// (permissions, disk full, whatever) leaves patch unsigned — a warning
// on stderr, not a failed record. record is the single most-invoked
// command; blocking it on an unrelated identity problem for a field
// that's opportunistic, not required, would be a real regression. An
// unsigned patch is a fully legitimate state — see
// Patch.VerifyAuthorSignature.
//
// Normalizes patch first — this is load-bearing, not just tidiness:
// Store.Put also calls Normalize (sorting Dependencies/Changes) right
// before Encode, so signing an un-normalized patch computes a signature
// over different bytes than what's actually stored and later verified,
// the moment there's more than one Dependency or Change to reorder — a
// two-way merge's Changes usually didn't expose this (often zero or one
// entry), but a real N-way apply's multi-dependency merge patch did,
// caught by live testing (Fingerprint showed "INVALID SIGNATURE" on a
// clean three-way apply). Normalize is idempotent, so Store.Put's own
// call afterward is a harmless no-op.
func signPatch(patch *patches.Patch) {
	patch.Normalize()
	id, err := auth.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: recording unsigned (identity unavailable): %v\n", err)
		return
	}
	copy(patch.AuthorFingerprint[:], id.Key.Public().(ed25519.PublicKey))
	sig := ed25519.Sign(id.Key, patch.SignablePayload())
	copy(patch.AuthorSignature[:], sig)
}
