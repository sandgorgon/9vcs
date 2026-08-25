// Package bundle implements signed, offline patch exchange: exporting a
// self-contained closure of patches (plus any blob content they
// reference) to a single file, signed by the exporting install's
// identity, and decoding/verifying one elsewhere — see PLAN.md decision
// #8, "Bundle export/import — concrete scope".
//
// A bundle's signature is a transport-provenance concern (who assembled
// and sent this file), separate from each patch's own AuthorFingerprint/
// AuthorSignature (who actually wrote that change, verified
// independently) — a bundle can legitimately carry patches authored by
// people other than whoever signed and sent it.
package bundle

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/sandgorgon/9vcs/objstore/patches"
)

var fileMagic = [4]byte{'9', 'V', 'C', 'B'}

// formatVersion tags Encode's output, same tripwire role as
// objstore/patches's patchFormatVersion: this project is pre-release
// with no bundle file anywhere whose decodability needs protecting, so
// there's deliberately only one recognized value here, not real
// multi-version dispatch. Revisit only once there's both a formal
// release and an actual need to change this format afterward.
const formatVersion byte = 1

// Bundle is a decoded, verifiable .9vp file: a signed, self-contained
// set of patches (plus any blob content they reference).
type Bundle struct {
	SignerPub ed25519.PublicKey
	Signature []byte
	Message   string
	Patches   []*patches.Patch
	Blobs     map[patches.Hash][]byte

	// payload is the exact bytes Signature was computed over, as read
	// off the wire. Verify checks against this directly — no re-encode
	// round-trip needed, same content-addressing philosophy as
	// everywhere else in this design: a patch is trusted because it
	// hashes right, a bundle because it verifies right.
	payload []byte
}

// Verify reports whether Signature genuinely matches SignerPub over the
// bundle's payload.
func (b *Bundle) Verify() bool {
	return ed25519.Verify(b.SignerPub, b.payload, b.Signature)
}

// Store writes every patch and blob in b into store/blobs. Content
// addressing makes this naturally idempotent — no separate hash-pinning
// needed, Store.Put/BlobStore.Put re-derive their hash from content on
// the way in regardless, same as every other write path. Store does not
// touch any ref: nothing is integrated until a human reviews the bundle
// and selectively applies patches from it.
//
// Every patch's own AuthorFingerprint/AuthorSignature is verified before
// anything is persisted — a separate question from the bundle's own
// signature (Verify, checked by the caller before Store is ever
// reached): the bundle signature only proves who sent this file, not
// that each individual patch's authorship claim inside it is genuine.
// Checked up front, before any write, so a bundle carrying even one
// forged claim is refused wholesale rather than partially imported —
// Store either fully succeeds or leaves nothing behind to review.
func (b *Bundle) Store(store *patches.Store, blobs *patches.BlobStore) error {
	for _, p := range b.Patches {
		if !p.VerifyAuthorSignature() {
			return fmt.Errorf("bundle: patch %s claims authorship by fingerprint %x but its signature doesn't verify — possible forgery", p.Hash(), p.AuthorFingerprint)
		}
	}
	for _, p := range b.Patches {
		if _, err := store.Put(p); err != nil {
			return fmt.Errorf("bundle: storing patch: %w", err)
		}
	}
	for _, data := range b.Blobs {
		if _, err := blobs.Put(data); err != nil {
			return fmt.Errorf("bundle: storing blob: %w", err)
		}
	}
	return nil
}

// Export builds a signed bundle containing every patch in the union of
// roots' dependency closures (patches.Closure is already variadic, so an
// arbitrary multi-root selection unions for free), plus any KindBlob
// content those patches reference, signed by signerKey. Returns the
// encoded file bytes and how many patches were included.
func Export(store *patches.Store, blobs *patches.BlobStore, roots []patches.Hash, message string, signerKey ed25519.PrivateKey) (data []byte, patchCount int, err error) {
	closure, err := patches.Closure(store, roots...)
	if err != nil {
		return nil, 0, err
	}

	pset := make([]*patches.Patch, 0, len(closure))
	blobSet := map[patches.Hash][]byte{}
	for h := range closure {
		p, err := store.Get(h)
		if err != nil {
			return nil, 0, err
		}
		pset = append(pset, p)
		for _, fc := range p.Changes {
			if fc.Kind != patches.KindBlob {
				continue
			}
			if _, ok := blobSet[fc.Blob]; ok {
				continue
			}
			data, err := blobs.Get(fc.Blob)
			if err != nil {
				return nil, 0, fmt.Errorf("bundle: reading blob %s referenced by patch %s: %w", fc.Blob, h, err)
			}
			blobSet[fc.Blob] = data
		}
	}
	// Deterministic order: same selection always produces the same
	// bundle bytes, not required for correctness (every patch/blob is
	// independently content-addressed on the way back in) but makes the
	// format testable and reviewable.
	sort.Slice(pset, func(i, j int) bool {
		hi, hj := pset[i].Hash(), pset[j].Hash()
		return bytes.Compare(hi[:], hj[:]) < 0
	})

	payload := encodePayload(message, pset, blobSet)
	signature := ed25519.Sign(signerKey, payload)
	signerPub := signerKey.Public().(ed25519.PublicKey)

	var buf bytes.Buffer
	buf.Write(fileMagic[:])
	buf.WriteByte(formatVersion)
	buf.Write(signerPub)
	buf.Write(signature)
	buf.Write(payload)
	return buf.Bytes(), len(pset), nil
}

func encodePayload(message string, pset []*patches.Patch, blobs map[patches.Hash][]byte) []byte {
	var buf bytes.Buffer
	writeString(&buf, message)
	writeInt64(&buf, int64(len(pset)))
	for _, p := range pset {
		data := p.Encode()
		writeInt64(&buf, int64(len(data)))
		buf.Write(data)
	}

	blobHashes := make([]patches.Hash, 0, len(blobs))
	for h := range blobs {
		blobHashes = append(blobHashes, h)
	}
	sort.Slice(blobHashes, func(i, j int) bool { return bytes.Compare(blobHashes[i][:], blobHashes[j][:]) < 0 })
	writeInt64(&buf, int64(len(blobHashes)))
	for _, h := range blobHashes {
		buf.Write(h[:])
		data := blobs[h]
		writeInt64(&buf, int64(len(data)))
		buf.Write(data)
	}
	return buf.Bytes()
}

// Decode parses a .9vp file's bytes into a Bundle. It does not verify
// the signature — call Verify explicitly, the same "decode, then
// verify, then decide whether to trust it" separation patches.Decode
// keeps from Store.Put's callers.
func Decode(data []byte) (*Bundle, error) {
	r := bytes.NewReader(data)
	var gotMagic [4]byte
	if _, err := io.ReadFull(r, gotMagic[:]); err != nil {
		return nil, fmt.Errorf("bundle: reading magic: %w", err)
	}
	if gotMagic != fileMagic {
		return nil, fmt.Errorf("bundle: not a 9vcs bundle file")
	}
	v, err := r.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("bundle: reading format version: %w", err)
	}
	if v != formatVersion {
		return nil, fmt.Errorf("bundle: unrecognized format version %d (want %d)", v, formatVersion)
	}

	signerPub := make([]byte, ed25519.PublicKeySize)
	if _, err := io.ReadFull(r, signerPub); err != nil {
		return nil, fmt.Errorf("bundle: reading signer public key: %w", err)
	}
	signature := make([]byte, ed25519.SignatureSize)
	if _, err := io.ReadFull(r, signature); err != nil {
		return nil, fmt.Errorf("bundle: reading signature: %w", err)
	}
	payload, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("bundle: reading payload: %w", err)
	}

	b := &Bundle{SignerPub: ed25519.PublicKey(signerPub), Signature: signature, payload: payload}

	pr := bytes.NewReader(payload)
	message, err := readString(pr)
	if err != nil {
		return nil, fmt.Errorf("bundle: decoding message: %w", err)
	}
	b.Message = message

	nPatches, err := readCount(pr, "bundle: decoding patch count")
	if err != nil {
		return nil, err
	}
	b.Patches = make([]*patches.Patch, 0, nPatches)
	for i := int64(0); i < nPatches; i++ {
		n, err := readCount(pr, fmt.Sprintf("bundle: decoding patch %d length", i))
		if err != nil {
			return nil, err
		}
		raw := make([]byte, n)
		if _, err := io.ReadFull(pr, raw); err != nil {
			return nil, fmt.Errorf("bundle: decoding patch %d: %w", i, err)
		}
		p, err := patches.Decode(raw)
		if err != nil {
			return nil, fmt.Errorf("bundle: decoding patch %d: %w", i, err)
		}
		b.Patches = append(b.Patches, p)
	}

	nBlobs, err := readCount(pr, "bundle: decoding blob count")
	if err != nil {
		return nil, err
	}
	b.Blobs = make(map[patches.Hash][]byte, nBlobs)
	for i := int64(0); i < nBlobs; i++ {
		var h patches.Hash
		if _, err := io.ReadFull(pr, h[:]); err != nil {
			return nil, fmt.Errorf("bundle: decoding blob %d hash: %w", i, err)
		}
		n, err := readCount(pr, fmt.Sprintf("bundle: decoding blob %d length", i))
		if err != nil {
			return nil, err
		}
		blobData := make([]byte, n)
		if _, err := io.ReadFull(pr, blobData); err != nil {
			return nil, fmt.Errorf("bundle: decoding blob %d: %w", i, err)
		}
		b.Blobs[h] = blobData
	}

	return b, nil
}

func readString(r *bytes.Reader) (string, error) {
	n, err := readCount(r, "bundle: decoding string length")
	if err != nil {
		return "", err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// readCount reads a length-prefixed count or size and validates it
// against how many bytes actually remain in r before returning — see
// objstore/patches's identical helper for the full rationale. Every
// count that goes on to size a make() in this package is read through
// this, not readInt64 directly, so a corrupted or adversarial .9vp file
// produces a clean decode error instead of an out-of-range allocation
// panic.
func readCount(r *bytes.Reader, what string) (int64, error) {
	n, err := readInt64(r)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	if n < 0 || n > int64(r.Len()) {
		return 0, fmt.Errorf("%s: implausible length %d (%d bytes remain)", what, n, r.Len())
	}
	return n, nil
}

func readInt64(r *bytes.Reader) (int64, error) {
	var tmp [8]byte
	if _, err := io.ReadFull(r, tmp[:]); err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(tmp[:])), nil
}

func writeString(buf *bytes.Buffer, s string) {
	writeInt64(buf, int64(len(s)))
	buf.WriteString(s)
}

func writeInt64(buf *bytes.Buffer, v int64) {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], uint64(v))
	buf.Write(tmp[:])
}
