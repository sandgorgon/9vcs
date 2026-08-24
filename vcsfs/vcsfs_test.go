package vcsfs

import (
	"io"
	"net"
	"sort"
	"testing"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"
	"github.com/sandgorgon/9p/server"

	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

type fakeRefs map[string]patches.Hash

func (r fakeRefs) RefHash(name string) (patches.Hash, bool, error) {
	h, ok := r[name]
	return h, ok, nil
}

func (r fakeRefs) ListRefs() ([]string, error) {
	names := make([]string, 0, len(r))
	for k := range r {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

func TestPatchesBlobsRefsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := patches.Open(dir + "/patches")
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := patches.OpenBlobs(dir + "/blobs")
	if err != nil {
		t.Fatal(err)
	}

	patch := &patches.Patch{Message: "test patch"}
	patchHash, err := store.Put(patch)
	if err != nil {
		t.Fatal(err)
	}
	blobHash, err := blobs.Put([]byte("blob content"))
	if err != nil {
		t.Fatal(err)
	}

	refs := fakeRefs{"main": patchHash}
	fs := &FS{Store: store, Blobs: blobs, Refs: refs, Perm: identity.PermRead}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &server.Server{FS: fs}
	go srv.Serve(ln)

	c, err := client.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Attach("test", ""); err != nil {
		t.Fatal(err)
	}

	patchFile, err := c.Open("patches/"+patchHash.String(), p9.OREAD)
	if err != nil {
		t.Fatalf("open patches/%s: %v", patchHash, err)
	}
	got, err := io.ReadAll(patchFile)
	if err != nil {
		t.Fatal(err)
	}
	patchFile.Close()
	want := patch.Encode()
	if string(got) != string(want) {
		t.Errorf("patch content mismatch: got %d bytes, want %d bytes", len(got), len(want))
	}
	// The bytes served must decode back to an equivalent patch and hash
	// the same — the actual property import relies on for integrity.
	decoded, err := patches.Decode(got)
	if err != nil {
		t.Fatalf("decoding served patch bytes: %v", err)
	}
	if decoded.Hash() != patchHash {
		t.Errorf("decoded patch hash = %s, want %s", decoded.Hash(), patchHash)
	}

	blobFile, err := c.Open("blobs/"+blobHash.String(), p9.OREAD)
	if err != nil {
		t.Fatalf("open blobs/%s: %v", blobHash, err)
	}
	gotBlob, err := io.ReadAll(blobFile)
	blobFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBlob) != "blob content" {
		t.Errorf("blob content = %q, want %q", gotBlob, "blob content")
	}

	refFile, err := c.Open("refs/main", p9.OREAD)
	if err != nil {
		t.Fatalf("open refs/main: %v", err)
	}
	gotRef, err := io.ReadAll(refFile)
	refFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(gotRef) != patchHash.String()+"\n" {
		t.Errorf("ref content = %q, want %q", gotRef, patchHash.String()+"\n")
	}

	if _, err := c.Open("refs/nonexistent", p9.OREAD); err == nil {
		t.Error("expected an error opening a nonexistent ref, got nil")
	}
	if _, err := c.Open("patches/not-a-hash", p9.OREAD); err == nil {
		t.Error("expected an error opening an invalid patch hash, got nil")
	}
}

func TestReadOnlyRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	store, err := patches.Open(dir + "/patches")
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := patches.OpenBlobs(dir + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	fs := &FS{Store: store, Blobs: blobs, Refs: fakeRefs{}, Perm: identity.PermWrite}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &server.Server{FS: fs}
	go srv.Serve(ln)

	c, err := client.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	root, err := c.Attach("test", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := root.Create("newfile", 0o644, p9.OWRITE); err == nil {
		t.Error("expected root.Create to fail (read-only), got nil")
	}
}
