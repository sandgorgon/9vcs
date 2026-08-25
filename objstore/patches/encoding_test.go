package patches

import (
	"crypto/ed25519"
	"crypto/rand"
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
