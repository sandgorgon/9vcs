package vcsfs

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"testing"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"
	"github.com/sandgorgon/9p/server"

	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

// dialWith starts a server for fs with the given ConnContext permission
// and returns a connected, attached client.
func dialWith(t *testing.T, fs *FS, perm identity.Permission) *client.Client {
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
	c := dialWith(t, fs, identity.PermWrite)

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
	c := dialWith(t, fs, identity.PermWrite)

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
	c := dialWith(t, fs, identity.PermWrite)

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
	c := dialWith(t, fs, identity.PermWrite)

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

func TestBlobWriteRoundTrip(t *testing.T) {
	fs, _ := newTestFS(t)
	c := dialWith(t, fs, identity.PermWrite)

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
	c := dialWith(t, fs, identity.PermRead)

	p := &patches.Patch{Message: "should be rejected"}
	_, err := c.Create("patches/"+p.Hash().String(), 0o644, p9.OWRITE)
	if err == nil {
		t.Error("expected permission denied for a read-only peer, got nil")
	}
}

func TestRefCreateNew(t *testing.T) {
	fs, refs := newTestFS(t)
	c := dialWith(t, fs, identity.PermWrite)

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
	c := dialWith(t, fs, identity.PermWrite)

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
	c := dialWith(t, fs, identity.PermWrite)

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

func TestRefCreateAlreadyExistsRejected(t *testing.T) {
	fs, refs := newTestFS(t)
	c := dialWith(t, fs, identity.PermWrite)

	existing, err := fs.Store.Put(&patches.Patch{Message: "existing"})
	if err != nil {
		t.Fatal(err)
	}
	refs["main"] = existing

	if _, err := c.Create("refs/main", 0o644, p9.OWRITE); err == nil {
		t.Error("expected Create on an already-existing ref to fail, matching normal 9P Tcreate semantics")
	}
}
