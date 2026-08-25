package bundle

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

func newTestStore(t *testing.T) (*patches.Store, *patches.BlobStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := patches.Open(dir + "/patches")
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := patches.OpenBlobs(dir + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	return store, blobs
}

func TestExportImportRoundTrip(t *testing.T) {
	store, blobs := newTestStore(t)

	base, err := store.Put(&patches.Patch{Message: "base"})
	if err != nil {
		t.Fatal(err)
	}
	blobHash, err := blobs.Put([]byte("binary content"))
	if err != nil {
		t.Fatal(err)
	}
	tip, err := store.Put(&patches.Patch{
		Dependencies: []patches.Hash{base},
		Message:      "adds a binary file",
		Changes:      []patches.FileChange{{Path: "logo.png", Kind: patches.KindBlob, Blob: blobHash}},
	})
	if err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, n, err := Export(store, blobs, []patches.Hash{tip}, "fix-parser bundle", priv)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("Export reported %d patches, want 2 (base + tip)", n)
	}

	b, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Verify() {
		t.Fatal("a bundle signed with the matching key should verify")
	}
	if string(b.SignerPub) != string(pub) {
		t.Error("decoded SignerPub doesn't match the signing key's public half")
	}
	if b.Message != "fix-parser bundle" {
		t.Errorf("Message = %q, want %q", b.Message, "fix-parser bundle")
	}
	if len(b.Patches) != 2 {
		t.Fatalf("got %d patches, want 2", len(b.Patches))
	}
	if len(b.Blobs) != 1 || string(b.Blobs[blobHash]) != "binary content" {
		t.Errorf("blob content did not round-trip: %v", b.Blobs)
	}

	// Store into a completely separate, empty store/blobs — the actual
	// import path.
	freshStore, freshBlobs := newTestStore(t)
	if err := b.Store(freshStore, freshBlobs); err != nil {
		t.Fatal(err)
	}
	if !freshStore.Has(base) || !freshStore.Has(tip) {
		t.Error("both patches should be present after Store")
	}
	if !freshBlobs.Has(blobHash) {
		t.Error("blob should be present after Store")
	}
	// Store must never touch a ref — there's no ref concept reachable
	// from Bundle.Store at all, but confirm the patches alone don't
	// materialize as a branch: nothing to check via a public API here
	// beyond "Store only ever calls Store.Put/BlobStore.Put", which the
	// implementation already guarantees by construction.
}

// TestExportUnionsMultipleRoots: patches.Closure is variadic, so
// exporting two independent, unrelated roots should include both
// closures' patches in one bundle.
func TestExportUnionsMultipleRoots(t *testing.T) {
	store, blobs := newTestStore(t)

	a, err := store.Put(&patches.Patch{Message: "branch A"})
	if err != nil {
		t.Fatal(err)
	}
	bHash, err := store.Put(&patches.Patch{Message: "branch B, unrelated"})
	if err != nil {
		t.Fatal(err)
	}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, n, err := Export(store, blobs, []patches.Hash{a, bHash}, "", priv)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("Export reported %d patches, want 2", n)
	}
	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[patches.Hash]bool{}
	for _, p := range decoded.Patches {
		seen[p.Hash()] = true
	}
	if !seen[a] || !seen[bHash] {
		t.Errorf("expected both unrelated roots' patches present, got %v", seen)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	store, blobs := newTestStore(t)
	tip, err := store.Put(&patches.Patch{Message: "original message"})
	if err != nil {
		t.Fatal(err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := Export(store, blobs, []patches.Hash{tip}, "bundle message", priv)
	if err != nil {
		t.Fatal(err)
	}

	// Flip a byte inside the payload (well past the fixed-size header:
	// magic+version+pubkey+signature), landing inside the bundle
	// message's own bytes — arbitrary content, so this can't corrupt the
	// length-prefixed framing itself, just the signed content.
	tampered := append([]byte(nil), data...)
	headerLen := len(fileMagic) + 1 + ed25519.PublicKeySize + ed25519.SignatureSize
	tampered[headerLen+8] ^= 0xFF // first byte of the message string's content

	decoded, err := Decode(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Verify() {
		t.Error("Verify should fail once the payload is tampered with after signing")
	}
}

func TestVerifyDetectsWrongKey(t *testing.T) {
	store, blobs := newTestStore(t)
	tip, err := store.Put(&patches.Patch{Message: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := Export(store, blobs, []patches.Hash{tip}, "", priv)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	decoded.SignerPub = otherPub
	if decoded.Verify() {
		t.Error("Verify should fail when SignerPub doesn't match the actual signing key")
	}
}

// TestStoreRejectsForgedPatchAuthorship: a bundle's own signature only
// proves who sent the file, not that each patch inside it genuinely
// authored what it claims — a patch with a fingerprint it doesn't hold
// the private key for must be refused by Store, independent of the
// bundle-level signature verifying just fine. All-or-nothing: no patch
// from the bundle should end up persisted, not just the forged one
// skipped.
func TestStoreRejectsForgedPatchAuthorship(t *testing.T) {
	store, blobs := newTestStore(t)

	good, err := store.Put(&patches.Patch{Message: "honest patch"})
	if err != nil {
		t.Fatal(err)
	}

	victimPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, attackerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forged := &patches.Patch{Dependencies: []patches.Hash{good}, Message: "forged patch"}
	copy(forged.AuthorFingerprint[:], victimPub)
	copy(forged.AuthorSignature[:], ed25519.Sign(attackerPriv, forged.SignablePayload()))
	forgedHash, err := store.Put(forged)
	if err != nil {
		t.Fatal(err)
	}

	_, bundlePriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := Export(store, blobs, []patches.Hash{forgedHash}, "", bundlePriv)
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Verify() {
		t.Fatal("the bundle-level signature should still verify — the forgery is inside one patch, not the bundle itself")
	}

	freshStore, freshBlobs := newTestStore(t)
	if err := decoded.Store(freshStore, freshBlobs); err == nil {
		t.Fatal("expected Store to refuse a bundle containing a forged patch authorship claim")
	}
	if freshStore.Has(good) || freshStore.Has(forgedHash) {
		t.Error("Store should be all-or-nothing: nothing should be persisted when any one patch fails verification")
	}
}

func TestDecodeRejectsBadMagic(t *testing.T) {
	if _, err := Decode([]byte("not a bundle at all, just text")); err == nil {
		t.Fatal("expected an error decoding non-bundle data, got nil")
	}
}

func TestDecodeRejectsUnrecognizedVersion(t *testing.T) {
	data := append([]byte{}, fileMagic[:]...)
	data = append(data, 99) // unrecognized version byte
	if _, err := Decode(data); err == nil {
		t.Fatal("expected an error decoding an unrecognized format version, got nil")
	}
}

// TestDecodeRejectsPatchCountNotBackedByMinEntrySize is a regression for
// an allocation-amplification gap: readCount used to validate a count
// only against total bytes remaining, not against how many bytes one
// element of the claimed count actually needs at minimum. A patch count
// that fit within the bytes remaining (so the old bound would have
// accepted it) but wasn't actually enough bytes to back that many
// entries — each needs at least its own 8-byte length prefix — used to
// pass, forcing `make([]*patches.Patch, 0, nPatches)` to over-allocate
// before the per-entry reads ran out of buffer and failed anyway. Now
// rejected immediately by readCount itself, before any allocation.
func TestDecodeRejectsPatchCountNotBackedByMinEntrySize(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(fileMagic[:])
	buf.WriteByte(formatVersion)
	buf.Write(make([]byte, ed25519.PublicKeySize))
	buf.Write(make([]byte, ed25519.SignatureSize))

	// Payload: an empty message, then a patch count of 50 backed by only
	// 50 bytes remaining. Each patch entry needs at least 8 bytes (its
	// own length prefix) to exist at all, so 50 entries need 400 bytes —
	// the old bound (50 <= 50 remaining) would have accepted this.
	var payload bytes.Buffer
	writeInt64(&payload, 0) // empty message
	writeInt64(&payload, 50)
	payload.Write(make([]byte, 50))
	buf.Write(payload.Bytes())

	if _, err := Decode(buf.Bytes()); err == nil {
		t.Fatal("expected an error decoding a patch count not backed by enough bytes for minPatchEntrySize, got nil")
	}
}
