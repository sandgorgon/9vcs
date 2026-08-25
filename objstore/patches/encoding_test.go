package patches

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"
	"time"
)

func samplePatch() *Patch {
	return &Patch{
		Dependencies: []Hash{{1, 2, 3}},
		Author:       "Ramon <ramondevera@gmail.com>",
		Time:         time.Unix(1700000000, 0).UTC(),
		Message:      "sample",
		Changes: []FileChange{
			{Path: "f.txt", Kind: KindText, TrailingNewline: true, Ops: []LineOp{
				{Kind: OpInsert, ID: "a", Prev: "", Next: "", Content: "hello"},
			}},
		},
	}
}

// TestEncodeDecodeRoundTrip: decoding an Encode'd patch and re-Encoding
// it must reproduce byte-for-byte identical output — Encode is a pure
// function of Patch's fields, not stateful. Covers both an unsigned
// patch and a signed one (AuthorFingerprint/AuthorSignature surviving
// the round trip is the part that's new since signing was added).
func TestEncodeDecodeRoundTrip(t *testing.T) {
	t.Run("unsigned", func(t *testing.T) {
		p := samplePatch()
		p.Normalize()
		original := p.Encode()

		decoded, err := Decode(original)
		if err != nil {
			t.Fatal(err)
		}
		reEncoded := decoded.Encode()
		if string(reEncoded) != string(original) {
			t.Fatalf("re-Encode produced different bytes:\noriginal:   %x\nre-encoded: %x", original, reEncoded)
		}
	})

	t.Run("signed", func(t *testing.T) {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		p := samplePatch()
		p.Normalize()
		copy(p.AuthorFingerprint[:], pub)
		copy(p.AuthorSignature[:], ed25519.Sign(priv, p.SignablePayload()))
		original := p.Encode()

		decoded, err := Decode(original)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.AuthorFingerprint != p.AuthorFingerprint {
			t.Errorf("AuthorFingerprint did not round-trip")
		}
		if decoded.AuthorSignature != p.AuthorSignature {
			t.Errorf("AuthorSignature did not round-trip")
		}
		reEncoded := decoded.Encode()
		if string(reEncoded) != string(original) {
			t.Fatalf("re-Encode produced different bytes:\noriginal:   %x\nre-encoded: %x", original, reEncoded)
		}
	})
}

// TestEncodeDecodeRoundTripExecutableAndSymlink pins format version 2's
// new fields: FileChange.Executable (on both KindText and KindBlob) and
// KindSymlink/SymlinkTarget.
func TestEncodeDecodeRoundTripExecutableAndSymlink(t *testing.T) {
	p := &Patch{
		Message: "adds a script, a compiled tool, and a symlink",
		Changes: []FileChange{
			{Path: "run.sh", Kind: KindText, TrailingNewline: true, Executable: true, Ops: []LineOp{
				{Kind: OpInsert, ID: "a", Content: "#!/bin/sh"},
			}},
			{Path: "bin/tool", Kind: KindBlob, Executable: true, Blob: Hash{9, 9, 9}},
			{Path: "bin/current", Kind: KindSymlink, SymlinkTarget: "tool-v2"},
		},
	}
	p.Normalize()
	original := p.Encode()

	decoded, err := Decode(original)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileChange{}
	for _, fc := range decoded.Changes {
		byPath[fc.Path] = fc
	}
	if !byPath["run.sh"].Executable {
		t.Error("run.sh: Executable did not round-trip as true")
	}
	if !byPath["bin/tool"].Executable {
		t.Error("bin/tool: Executable did not round-trip as true")
	}
	if byPath["bin/current"].Kind != KindSymlink {
		t.Errorf("bin/current: Kind = %v, want KindSymlink", byPath["bin/current"].Kind)
	}
	if byPath["bin/current"].SymlinkTarget != "tool-v2" {
		t.Errorf("bin/current: SymlinkTarget = %q, want %q", byPath["bin/current"].SymlinkTarget, "tool-v2")
	}

	reEncoded := decoded.Encode()
	if string(reEncoded) != string(original) {
		t.Fatalf("re-Encode produced different bytes:\noriginal:   %x\nre-encoded: %x", original, reEncoded)
	}
}

func TestEncodeStartsWithFormatByte(t *testing.T) {
	p := samplePatch()
	p.Normalize()
	encoded := p.Encode()
	if len(encoded) == 0 || encoded[0] != patchFormatVersion {
		t.Fatalf("first byte = %v, want patchFormatVersion (%d)", encoded[:min(1, len(encoded))], patchFormatVersion)
	}
}

func TestVerifyAuthorSignatureUnsignedIsValid(t *testing.T) {
	p := samplePatch()
	if !p.VerifyAuthorSignature() {
		t.Error("an unsigned patch (zero AuthorFingerprint) should verify as true — no claim made")
	}
}

func TestVerifyAuthorSignatureValid(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := samplePatch()
	copy(p.AuthorFingerprint[:], pub)
	copy(p.AuthorSignature[:], ed25519.Sign(priv, p.SignablePayload()))

	if !p.VerifyAuthorSignature() {
		t.Error("a correctly signed patch should verify as true")
	}
}

// TestVerifyAuthorSignatureDetectsTampering is the actual point of
// signing: any change to the signed content after signing — here, the
// message, but any field would do — must make verification fail. This is
// what a malicious relay forging a patch under someone else's claimed
// fingerprint would run into: it can't produce a signature that verifies
// against content it altered without the real private key.
func TestVerifyAuthorSignatureDetectsTampering(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := samplePatch()
	copy(p.AuthorFingerprint[:], pub)
	copy(p.AuthorSignature[:], ed25519.Sign(priv, p.SignablePayload()))

	p.Message = "tampered after signing"
	if p.VerifyAuthorSignature() {
		t.Error("verification should fail once signed content is altered")
	}
}

// TestVerifyAuthorSignatureWrongKey: a fingerprint that doesn't match the
// key that actually produced the signature must fail verification —
// forging a claim by writing someone else's fingerprint into an
// otherwise-honest patch shouldn't verify just because *a* signature is
// present.
func TestVerifyAuthorSignatureWrongKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := samplePatch()
	copy(p.AuthorFingerprint[:], otherPub) // claims otherPub, signed by a different key
	copy(p.AuthorSignature[:], ed25519.Sign(priv, p.SignablePayload()))

	if p.VerifyAuthorSignature() {
		t.Error("verification should fail when the claimed fingerprint doesn't match the signing key")
	}
}

// TestSigningMustHappenAfterNormalize is a regression for a real bug
// found via live testing of apply (a merge patch with several
// Dependencies): cmd/9vcs's record.go signed a patch before Store.Put's
// internal Normalize() reordered Dependencies/Changes, so the signed
// bytes and the later-verified bytes diverged the moment there was more
// than one Dependency to reorder — a two-way merge's single "theirs"
// dependency rarely exposed this, but a real N-way apply always
// involves several. This pins the property any future signing call
// site needs: sign only after Normalize, never before.
func TestSigningMustHappenAfterNormalize(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	unsorted := []Hash{{0xFF}, {0x01}} // deliberately not in Normalize's sorted order

	// The bug: sign before Normalize, exactly what record.go used to do.
	broken := &Patch{Dependencies: append([]Hash{}, unsorted...), Message: "m"}
	copy(broken.AuthorFingerprint[:], pub)
	copy(broken.AuthorSignature[:], ed25519.Sign(priv, broken.SignablePayload()))
	broken.Normalize() // Store.Put does this before Encode/Hash
	if broken.VerifyAuthorSignature() {
		t.Fatal("signing before Normalize should break verification once Normalize reorders Dependencies — this test documents the bug shape, not the fix")
	}

	// The fix: Normalize before signing (idempotent, so Store.Put's own
	// later Normalize call is a harmless no-op).
	fixed := &Patch{Dependencies: append([]Hash{}, unsorted...), Message: "m"}
	fixed.Normalize()
	copy(fixed.AuthorFingerprint[:], pub)
	copy(fixed.AuthorSignature[:], ed25519.Sign(priv, fixed.SignablePayload()))
	fixed.Normalize()
	if !fixed.VerifyAuthorSignature() {
		t.Fatal("signing after Normalize should verify even after a second, idempotent Normalize call")
	}
}

func TestDecodeRejectsUnrecognizedFormatByte(t *testing.T) {
	if _, err := Decode([]byte{0}); err == nil {
		t.Fatal("expected an error decoding format byte 0 (never issued), got nil")
	}
	if _, err := Decode([]byte{99}); err == nil {
		t.Fatal("expected an error decoding an unrecognized format byte, got nil")
	}
}

func TestDecodeEmptyData(t *testing.T) {
	if _, err := Decode(nil); err == nil {
		t.Fatal("expected an error decoding empty data, got nil")
	}
}

// TestDecodeRejectsImplausibleLength is a regression for a real crash
// found via live testing of bundle export/import: a corrupted or
// adversarial length-prefixed count (here, the dependency count) used to
// panic make() with "len out of range" instead of returning a decode
// error. Every length-prefixed count now goes through readCount, which
// bounds it against the bytes actually remaining before it's ever used
// to size an allocation.
func TestDecodeRejectsImplausibleLength(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteByte(patchFormatVersion)
	// A dependency count larger than any plausible input, well beyond
	// what's actually left in the buffer.
	var countBytes [8]byte
	binary.BigEndian.PutUint64(countBytes[:], 1<<62)
	buf.Write(countBytes[:])

	if _, err := Decode(buf.Bytes()); err == nil {
		t.Fatal("expected an error decoding an implausible dependency count, got nil")
	}
}
