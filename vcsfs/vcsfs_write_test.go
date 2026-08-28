package vcsfs

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"math"
	"net"
	"testing"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"
	"github.com/sandgorgon/9p/server"

	"github.com/sandgorgon/9auth"
	"github.com/sandgorgon/9vcs/bundle"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

// dialWith starts a server for fs with the given ConnContext permission
// and returns a connected, attached client.
func dialWith(t *testing.T, fs *FS, perm auth.Permission) *client.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &server.Server{FS: fs, ConnContext: connContextFor(perm)}
	go srv.Serve(ln)

	c, err := client.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Attach("test", ""); err != nil {
		t.Fatal(err)
	}
	return c
}

func newTestFS(t *testing.T) (*FS, fakeRefs) {
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
	refs := fakeRefs{}
	return &FS{Store: store, Blobs: blobs, Refs: refs}, refs
}

func TestPatchWriteRoundTrip(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	p := &patches.Patch{Message: "pushed patch"}
	data := p.Encode()
	hash := p.Hash()

	f, err := c.Create("patches/"+hash.String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close (finalize): %v", err)
	}

	// Read it back through the normal read path.
	rf, err := c.Open("patches/"+hash.String(), p9.OREAD)
	if err != nil {
		t.Fatalf("Open for read: %v", err)
	}
	got, err := io.ReadAll(rf)
	rf.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("read back %d bytes, want %d bytes matching what was written", len(got), len(data))
	}

	// And directly in the store, bypassing the network entirely.
	if !fs.Store.Has(hash) {
		t.Error("patch not present in the store after a successful push")
	}
}

// TestPatchWriteWrongHashRejected checks that content pushed under the
// wrong claimed hash never becomes reachable under that wrong name, and
// that the rejection reaches the client as Close's returned error
// (github.com/sandgorgon/9p v0.4.0+ propagates it through Tclunk — see
// TestCloseErrorsPropagateThroughDispatch).
func TestPatchWriteWrongHashRejected(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	p := &patches.Patch{Message: "real content"}
	realHash := p.Hash()
	claimedHash := patches.Hash{} // all-zero: definitely not p's real hash

	f, err := c.Create("patches/"+claimedHash.String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(p.Encode()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err == nil {
		t.Error("expected Close to report the hash mismatch, got nil")
	}

	// Not silently lost: it's stored under its own real hash regardless
	// (content addressing means there's nothing to "undo"), just not
	// reachable under the wrong name it was pushed as.
	if !fs.Store.Has(realHash) {
		t.Error("content should still be stored under its own real hash")
	}
	if fs.Store.Has(claimedHash) {
		t.Error("content should not be reachable under the wrong claimed hash")
	}
}

// TestPatchWriteForgedAuthorshipRejected: a patch claiming a fingerprint
// via a signature that doesn't actually verify against it (the "relay
// crafts a fake patch and claims it's from someone else" scenario the
// AuthorFingerprint/AuthorSignature design exists to catch — see
// PLAN.md decision #1) must be refused, and never become reachable
// under any hash — not stored under its self-consistent real hash the
// way TestPatchWriteWrongHashRejected's honest-but-misnamed content is,
// since this check runs before Store.Put at all.
func TestPatchWriteForgedAuthorshipRejected(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	victimPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, attackerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	p := &patches.Patch{Message: "forged patch"}
	copy(p.AuthorFingerprint[:], victimPub)                                     // claims the victim's identity
	copy(p.AuthorSignature[:], ed25519.Sign(attackerPriv, p.SignablePayload())) // but signed with a different key
	realHash := p.Hash()

	f, err := c.Create("patches/"+realHash.String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(p.Encode()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err == nil {
		t.Fatal("expected Close to report the forged authorship claim, got nil")
	}
	if fs.Store.Has(realHash) {
		t.Error("a forged authorship claim must not be stored at all, under any hash")
	}
}

// TestPatchWriteSignedAuthorshipAccepted: the honest counterpart — a
// patch genuinely signed by the fingerprint it claims must push through
// exactly like an unsigned one.
func TestPatchWriteSignedAuthorshipAccepted(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p := &patches.Patch{Message: "genuinely signed patch"}
	copy(p.AuthorFingerprint[:], pub)
	copy(p.AuthorSignature[:], ed25519.Sign(priv, p.SignablePayload()))
	hash := p.Hash()

	f, err := c.Create("patches/"+hash.String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(p.Encode()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !fs.Store.Has(hash) {
		t.Error("a genuinely signed patch should be stored like any other")
	}
}

// TestPatchWritePathTraversalRejected is the vcsfs-level regression test
// for a real, live-proven vulnerability (see PLAN.md's "Path traversal
// via FileChange.Path"): a patch whose FileChange.Path escapes the repo
// root via ".." reached this exact write path (a served peer pushing a
// patch — the real route a malicious or buggy import/reconcile source
// would use) and, before objstore/patches.Decode validated it, would
// have been accepted and, once later applied, written a file completely
// outside the recipient's repo. Refused here, at the same severity as a
// hash mismatch or a forged authorship claim, and — like those — never
// stored under any hash.
func TestPatchWritePathTraversalRejected(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	p := &patches.Patch{Message: "innocuous-looking commit message", Changes: []patches.FileChange{
		{Path: "../../../../outside-the-repo.txt", Kind: patches.KindText, TrailingNewline: true,
			Ops: []patches.LineOp{{Kind: patches.OpInsert, ID: "a", Content: "pwned"}}},
	}}
	realHash := p.Hash()

	f, err := c.Create("patches/"+realHash.String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(p.Encode()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err == nil {
		t.Fatal("expected Close to reject a path-traversal patch, got nil")
	}
	if fs.Store.Has(realHash) {
		t.Error("a patch with a path-traversal Path must not be stored, under any hash")
	}
}

// TestWriteRejectsOversizedOffset is the regression test for a real,
// live-proven vulnerability (see PLAN.md's "Unbounded write-offset
// allocation"): before checkWriteSize existed, a single tiny write
// claiming an enormous offset forced the server to allocate a buffer of
// that claimed size — proven live with a 2-byte write claiming a 400MB
// offset growing the server's heap by ~400MB, scaling without limit.
// Reachable before any content, hash, or signature is ever checked
// (that only happens at Close), by the lowest trust tier this server
// has (PermPropose, via /offers), not just PermWrite.
func TestWriteRejectsOversizedOffset(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	p := &patches.Patch{Message: "irrelevant — rejected before content is ever read"}
	f, err := c.Create("patches/"+p.Hash().String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A tiny write, at an offset just past MaxObjectSize — real data
	// sent is a couple of bytes; what's under test is whether the
	// server still tries to honor the claimed offset regardless.
	if _, err := f.WriteAt([]byte("hi"), MaxObjectSize+1); err == nil {
		t.Error("expected a write past MaxObjectSize to be rejected, got nil")
	}
}

// TestWriteRejectsOverflowingOffset is the regression test for a real,
// crash-level vulnerability: checkWriteSize computed offset+len(p)
// without first checking offset alone against MaxObjectSize, so an
// offset near math.MaxInt64 made that addition overflow and wrap around
// to a large *negative* number — which then passed the `end >
// MaxObjectSize` check (a negative number is never greater than a
// positive limit), silently accepting the write. The caller then did
// buf[offset:] with that same huge offset against a small buffer, which
// panics — and every 9P request runs in its own goroutine with no panic
// recovery anywhere in github.com/sandgorgon/9p's server package, so
// this crashed the entire server process (every connection, not just
// the offending one), reachable by any connected peer at even the
// weakest trust tier. Verified directly against checkWriteSize, not
// through the network — the panic this guards against would kill the
// test process itself, not just fail the test.
func TestWriteRejectsOverflowingOffset(t *testing.T) {
	if err := checkWriteSize(math.MaxInt64-5, []byte("0123456789")); err == nil {
		t.Error("expected an offset near math.MaxInt64 (whose sum with len(p) overflows) to be rejected, got nil")
	}
	if err := checkWriteSize(math.MaxInt64, nil); err == nil {
		t.Error("expected offset == math.MaxInt64 to be rejected even with an empty payload, got nil")
	}
}

func TestWriteRejectsNegativeOffset(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	p := &patches.Patch{Message: "irrelevant"}
	f, err := c.Create("patches/"+p.Hash().String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A negative offset would otherwise reach the later buf[offset:]
	// slice expression directly and panic — Go slicing with a negative
	// index always panics, unconditionally. Reachable in practice when a
	// peer sends an unsigned 9P offset large enough to wrap into a
	// negative int64.
	if _, err := f.WriteAt([]byte("hi"), -1); err == nil {
		t.Error("expected a negative-offset write to be rejected, got nil")
	}
}

// dialWithBudget is dialWith, but also attaches a small per-connection
// write-buffer budget via WithWriteBudget — for exercising the
// MaxObjectSize-bounds-one-fid-but-nothing-bounded-the-connection-total
// gap fixed alongside it (see writeBudget's doc comment in vcsfs.go).
func dialWithBudget(t *testing.T, fs *FS, perm auth.Permission, budget int64) *client.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	srv := &server.Server{FS: fs, ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
		return WithWriteBudget(WithPermission(ctx, perm), budget)
	}}
	go srv.Serve(ln)

	c, err := client.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Attach("test", ""); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestWriteBudgetRejectsExceedingConnectionTotal is the regression test
// for a real gap: MaxObjectSize alone bounds one fid's buffer, but
// nothing previously bounded how many fids a single connection could
// hold open (buffering) at once — a client could Create many fids and
// write close to MaxObjectSize into each without ever Tclunk-ing any of
// them, live-proven at ~100 fids x ~1GiB. With a connection-wide budget
// attached, a second fid's write that would push the connection's total
// buffered bytes over budget must be rejected, even though neither fid
// alone is anywhere near MaxObjectSize.
func TestWriteBudgetRejectsExceedingConnectionTotal(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWithBudget(t, fs, auth.PermWrite, 100)

	p1 := &patches.Patch{Message: "first"}
	f1, err := c.Create("patches/"+p1.Hash().String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create f1: %v", err)
	}
	if _, err := f1.Write(make([]byte, 60)); err != nil {
		t.Fatalf("f1 write within budget: %v", err)
	}

	p2 := &patches.Patch{Message: "second (different content, different hash)"}
	f2, err := c.Create("patches/"+p2.Hash().String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create f2: %v", err)
	}
	if _, err := f2.Write(make([]byte, 60)); err == nil {
		t.Error("expected f2's write to be rejected — f1 (60 bytes) + f2 (60 bytes) exceeds the 100-byte connection budget")
	}
}

// TestWriteBudgetReleasedOnClose confirms Close (Tclunk) frees a fid's
// reservation back to its connection's budget, so a finished write
// doesn't permanently eat into what a later one on the same connection
// can use.
func TestWriteBudgetReleasedOnClose(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWithBudget(t, fs, auth.PermWrite, 100)

	p1 := &patches.Patch{Message: "first"}
	f1, err := c.Create("patches/"+p1.Hash().String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create f1: %v", err)
	}
	if _, err := f1.Write(make([]byte, 60)); err != nil {
		t.Fatalf("f1 write within budget: %v", err)
	}
	f1.Close() // ignoring the error: 60 zero bytes isn't a valid encoded patch, but the budget release happens regardless of Close's outcome

	p2 := &patches.Patch{Message: "second (different content, different hash)"}
	f2, err := c.Create("patches/"+p2.Hash().String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create f2: %v", err)
	}
	if _, err := f2.Write(make([]byte, 60)); err != nil {
		t.Errorf("expected f2's write to succeed after f1's budget was released by Close, got %v", err)
	}
}

func TestBlobWriteRoundTrip(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	data := []byte("some binary-ish content")
	// Compute the real hash independently, via a throwaway store — using
	// fs.Blobs itself to compute it would defeat the point of the test
	// (storing it locally before ever pushing it over the wire).
	tmp := t.TempDir()
	scratch, err := patches.OpenBlobs(tmp)
	if err != nil {
		t.Fatal(err)
	}
	wantHash, err := scratch.Put(data)
	if err != nil {
		t.Fatal(err)
	}

	f, err := c.Create("blobs/"+wantHash.String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close (finalize): %v", err)
	}

	got, err := fs.Blobs.Get(wantHash)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("stored blob = %q, want %q", got, data)
	}
}

func TestWriteRequiresPermission(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, auth.PermRead)

	p := &patches.Patch{Message: "should be rejected"}
	_, err := c.Create("patches/"+p.Hash().String(), 0o644, p9.OWRITE)
	if err == nil {
		t.Error("expected permission denied for a read-only peer, got nil")
	}
}

func TestRefCreateNew(t *testing.T) {
	fs, refs := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	target, err := fs.Store.Put(&patches.Patch{Message: "target"})
	if err != nil {
		t.Fatal(err)
	}

	f, err := c.Create("refs/main", 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	payload := patches.Hash{}.String() + " " + target.String() + "\n"
	if _, err := f.Write([]byte(payload)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, ok := refs["main"]
	if !ok || got != target {
		t.Errorf("refs[main] = %v, %v, want %s, true", got, ok, target)
	}
}

func TestRefUpdateExistingAndCASConflict(t *testing.T) {
	fs, refs := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	oldHash, err := fs.Store.Put(&patches.Patch{Message: "old"})
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := fs.Store.Put(&patches.Patch{Message: "new"})
	if err != nil {
		t.Fatal(err)
	}
	refs["main"] = oldHash

	// Wrong expected-old: must fail, and must not change the ref.
	badF, err := c.Open("refs/main", p9.OWRITE)
	if err != nil {
		t.Fatalf("Open for write: %v", err)
	}
	wrongOld := newHash // deliberately not the actual current value (oldHash)
	if _, err := badF.Write([]byte(wrongOld.String() + " " + newHash.String() + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := badF.Close(); err == nil {
		t.Error("expected Close to report the CAS conflict, got nil")
	}
	if refs["main"] != oldHash {
		t.Errorf("ref changed despite a rejected CAS write: got %s, want unchanged %s", refs["main"], oldHash)
	}

	// Correct expected-old: must succeed.
	goodF, err := c.Open("refs/main", p9.OWRITE)
	if err != nil {
		t.Fatalf("Open for write: %v", err)
	}
	if _, err := goodF.Write([]byte(oldHash.String() + " " + newHash.String() + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := goodF.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if refs["main"] != newHash {
		t.Errorf("refs[main] = %s, want %s", refs["main"], newHash)
	}
}

// TestCloseErrorsPropagateThroughDispatch pins a fix landed in
// github.com/sandgorgon/9p v0.4.0 (see CHANGELOG.md's v0.4.0 entry):
// server.File.Close() returns an error per its interface signature, and
// conn.tClunk (server/dispatch.go) now reports it to the client as an
// Rerror instead of discarding it and always replying with a bare
// Rclunk, as earlier versions did (see git history for the prior version
// of this test, TestCloseErrorsAreDiscardedByDispatch, and
// cmd/9vcs/reconcile.go's git history for the read-back-verification
// workaround this fix let us drop from the push path).
func TestCloseErrorsPropagateThroughDispatch(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	// Any Close() that returns a non-nil error demonstrates this — reuse
	// the wrong-claimed-hash case, which writeFile.Close reliably rejects.
	p := &patches.Patch{Message: "content"}
	wrongName := patches.Hash{}

	f, err := c.Create("patches/"+wrongName.String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(p.Encode()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err == nil {
		t.Fatal("expected Close to report the hash mismatch as an error, got nil")
	}
}

// newTestFSWithOffers is newTestFS plus an Offers store — kept separate
// from newTestFS (which leaves Offers nil) so the nil case stays
// independently tested: an FS with no Offers store behaves exactly as it
// did before this feature existed (see TestOffersAbsentWhenNil).
func newTestFSWithOffers(t *testing.T) (*FS, fakeRefs) {
	t.Helper()
	fs, refs := newTestFS(t)
	dir := t.TempDir()
	offers, err := patches.OpenBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	fs.Offers = offers
	return fs, refs
}

// newSignedBundle builds valid, verifiably-signed bundle bytes containing
// one throwaway patch — enough for offer tests, which only need bytes
// that satisfy bundle.Decode + Bundle.Verify, not a bundle with any
// particular content.
func newSignedBundle(t *testing.T) []byte {
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
	h, err := store.Put(&patches.Patch{Message: "offer content"})
	if err != nil {
		t.Fatal(err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := bundle.Export(store, blobs, []patches.Hash{h}, "an offer", priv)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOfferPostRoundTrip(t *testing.T) {
	fs, _ := newTestFSWithOffers(t)
	c := dialWith(t, fs, auth.PermPropose)

	data := newSignedBundle(t)
	id := patches.Hash(sha256.Sum256(data))

	f, err := c.Create("offers/"+id.String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close (finalize): %v", err)
	}

	if !fs.Offers.Has(id) {
		t.Error("offer not present in the offers store after a successful post")
	}

	rf, err := c.Open("offers/"+id.String(), p9.OREAD)
	if err != nil {
		t.Fatalf("Open for read: %v", err)
	}
	got, err := io.ReadAll(rf)
	rf.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("read back %d bytes, want %d bytes matching what was posted", len(got), len(data))
	}
}

// TestOfferPostRequiresProposePermission: a peer with only PermRead can't
// post — PermPropose is the minimum, one tier narrower than PermWrite
// everything else under Create requires.
func TestOfferPostRequiresProposePermission(t *testing.T) {
	fs, _ := newTestFSWithOffers(t)
	c := dialWith(t, fs, auth.PermRead)

	data := newSignedBundle(t)
	id := patches.Hash(sha256.Sum256(data))
	if _, err := c.Create("offers/"+id.String(), 0o644, p9.OWRITE); err == nil {
		t.Error("expected permission denied for a read-only peer posting an offer, got nil")
	}
}

// TestOfferPostInvalidSignatureRejected: an offer whose own bundle
// signature doesn't verify — garbage, or tampered after signing — must
// be refused outright, never reaching the offers store at all. This is a
// different check from each inner patch's own AuthorFingerprint/
// AuthorSignature (deliberately deferred to Bundle.Store at `offer
// apply` time, not checked here) — this one is about the bundle itself
// being trustworthy enough to sit in the queue.
func TestOfferPostInvalidSignatureRejected(t *testing.T) {
	fs, _ := newTestFSWithOffers(t)
	c := dialWith(t, fs, auth.PermPropose)

	data := newSignedBundle(t)
	tampered := append([]byte(nil), data...)
	tampered[len(tampered)-1] ^= 0xFF // corrupt the last payload byte, signature no longer verifies
	id := patches.Hash(sha256.Sum256(tampered))

	f, err := c.Create("offers/"+id.String(), 0o644, p9.OWRITE)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.Write(tampered); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := f.Close(); err == nil {
		t.Fatal("expected Close to report the invalid bundle signature, got nil")
	}
	if fs.Offers.Has(id) {
		t.Error("an offer with an invalid signature must not be stored")
	}
}

func TestOfferListing(t *testing.T) {
	fs, _ := newTestFSWithOffers(t)
	data1 := newSignedBundle(t)
	data2 := newSignedBundle(t)
	id1, err := fs.Offers.Put(data1)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := fs.Offers.Put(data2)
	if err != nil {
		t.Fatal(err)
	}

	c := dialWith(t, fs, auth.PermRead)
	f, err := c.Open("offers", p9.OREAD)
	if err != nil {
		t.Fatalf("Open offers dir: %v", err)
	}
	defer f.Close()
	stats, err := f.ReadDir()
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(stats) != 2 {
		t.Fatalf("got %d entries, want 2", len(stats))
	}
	seen := map[string]bool{}
	for _, st := range stats {
		seen[st.Name] = true
	}
	if !seen[id1.String()] || !seen[id2.String()] {
		t.Errorf("offer listing = %v, want both %s and %s", seen, id1, id2)
	}
}

// TestOffersAbsentWhenNil: an FS with no Offers store (the zero value,
// exactly what every FS built before this feature existed already looks
// like) behaves as if /offers doesn't exist at all — no panic, no
// special-cased error, just an ordinary "no such file."
func TestOffersAbsentWhenNil(t *testing.T) {
	fs, _ := newTestFS(t) // Offers left nil
	c := dialWith(t, fs, auth.PermRead)

	if _, err := c.Open("offers", p9.OREAD); err == nil {
		t.Error("expected opening /offers to fail when FS.Offers is nil, got nil")
	}
}

func TestOfferRemoveRequiresWritePermission(t *testing.T) {
	fs, _ := newTestFSWithOffers(t)
	data := newSignedBundle(t)
	id, err := fs.Offers.Put(data)
	if err != nil {
		t.Fatal(err)
	}

	// PermPropose can post but not remove — remove needs PermWrite.
	c := dialWith(t, fs, auth.PermPropose)
	root, err := c.Attach("test", "")
	if err != nil {
		t.Fatal(err)
	}
	offerFid, err := root.Walk("offers", id.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := offerFid.Remove(); err == nil {
		t.Error("expected Remove to be refused for a propose-only peer, got nil")
	}
	if !fs.Offers.Has(id) {
		t.Error("offer should still be present after a refused Remove")
	}
}

func TestOfferRemove(t *testing.T) {
	fs, _ := newTestFSWithOffers(t)
	data := newSignedBundle(t)
	id, err := fs.Offers.Put(data)
	if err != nil {
		t.Fatal(err)
	}

	c := dialWith(t, fs, auth.PermWrite)
	root, err := c.Attach("test", "")
	if err != nil {
		t.Fatal(err)
	}
	offerFid, err := root.Walk("offers", id.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := offerFid.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if fs.Offers.Has(id) {
		t.Error("offer should be gone after a successful Remove")
	}
}

func TestRefCreateAlreadyExistsRejected(t *testing.T) {
	fs, refs := newTestFS(t)
	c := dialWith(t, fs, auth.PermWrite)

	existing, err := fs.Store.Put(&patches.Patch{Message: "existing"})
	if err != nil {
		t.Fatal(err)
	}
	refs["main"] = existing

	if _, err := c.Create("refs/main", 0o644, p9.OWRITE); err == nil {
		t.Error("expected Create on an already-existing ref to fail, matching normal 9P Tcreate semantics")
	}
}
