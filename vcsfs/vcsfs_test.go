package vcsfs

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"testing"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"
	"github.com/sandgorgon/9p/server"

	"github.com/sandgorgon/9auth"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

// connContextFor builds a Server.ConnContext that unconditionally attaches
// perm — these tests aren't exercising authentication (identity's own
// tests already cover that), just vcsfs's wiring, so there's no peer
// identity to derive it from.
func connContextFor(perm auth.Permission) func(context.Context, net.Conn) context.Context {
	return func(ctx context.Context, _ net.Conn) context.Context {
		return WithPermission(ctx, perm)
	}
}

type fakeRefs map[string]patches.Hash

func (r fakeRefs) RefHash(name string) (patches.Hash, bool, error) {
	h, ok := r[name]
	return h, ok, nil
}

func (r fakeRefs) SetRefHash(name string, old, new patches.Hash) error {
	current, exists := r[name]
	if exists {
		if current != old {
			return fmt.Errorf("ref conflict: %q is at %s, not %s", name, current, old)
		}
	} else if !old.IsZero() {
		return fmt.Errorf("ref conflict: %q does not exist, expected %s", name, old)
	}
	r[name] = new
	return nil
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
	fs := &FS{Store: store, Blobs: blobs, Refs: refs}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &server.Server{FS: fs, ConnContext: connContextFor(auth.PermRead)}
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

func manyRefs(t *testing.T, store *patches.Store, n int) (fakeRefs, []string) {
	t.Helper()
	refs := fakeRefs{}
	var want []string
	for i := range n {
		name := fmt.Sprintf("branch-with-a-somewhat-long-name-%03d", i)
		h, err := store.Put(&patches.Patch{Message: name})
		if err != nil {
			t.Fatal(err)
		}
		refs[name] = h
		want = append(want, name)
	}
	sort.Strings(want)
	return refs, want
}

// TestDirectoryListingPaginates exercises the bug MarshalDir fixed on the
// server side: a listing that doesn't fit in one buffer must still come
// back complete across multiple Read calls at growing offsets — the
// pre-v0.2.0 dirFile.Read here only ever handled offset 0, silently
// truncating anything larger. This drives dirFile.Read directly (no
// client involved) the way a *correct* 9P directory-read client actually
// behaves: keep reading at offset += n until a read returns exactly zero
// bytes — not merely fewer bytes than requested. See
// TestDirectoryListingOverTheWireHitsAClientBug for why that distinction
// matters: this library's own client doesn't currently make it.
func TestDirectoryListingPaginates(t *testing.T) {
	dir := t.TempDir()
	store, err := patches.Open(dir + "/patches")
	if err != nil {
		t.Fatal(err)
	}
	refs, want := manyRefs(t, store, 50)
	d := &dirFile{fs: &FS{Store: store, Refs: refs}, kind: kindRefs}

	var got []string
	var offset int64
	buf := make([]byte, 100) // small on purpose, forces multiple rounds
	for rounds := 0; ; rounds++ {
		if rounds > len(want)+2 {
			t.Fatal("more rounds than entries — not converging, likely an infinite loop")
		}
		n, err := d.Read(context.Background(), offset, buf)
		if err != nil && err != io.EOF {
			t.Fatalf("Read at offset %d: %v", offset, err)
		}
		if n > 0 {
			for chunk := buf[:n]; len(chunk) > 0; {
				st, rest := decodeOneStat(t, chunk)
				got = append(got, st.Name)
				chunk = rest
			}
			offset += int64(n)
		}
		if n == 0 {
			break // the correct end-of-directory signal: an empty read, not merely a short one
		}
	}

	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// decodeOneStat peels one Stat blob (2-byte little-endian size prefix +
// fields, exactly Stat.Marshal's format) off the front of chunk.
func decodeOneStat(t *testing.T, chunk []byte) (p9.Stat, []byte) {
	t.Helper()
	if len(chunk) < 2 {
		t.Fatalf("chunk too short for a size prefix: %d bytes", len(chunk))
	}
	size := int(chunk[0]) | int(chunk[1])<<8
	total := 2 + size
	if total > len(chunk) {
		t.Fatalf("entry claims %d bytes, only %d available — a split entry, exactly what MarshalDir must never produce", total, len(chunk))
	}
	st, err := p9.UnmarshalStat(chunk[:total])
	if err != nil {
		t.Fatalf("UnmarshalStat: %v", err)
	}
	return st, chunk[total:]
}

// TestDirectoryListingOverTheWire is a regression test for a real bug
// found in github.com/sandgorgon/9p v0.2.0 while adopting MarshalDir
// here: client.File.ReadDir silently truncated any listing that didn't
// fit in one negotiated-msize buffer, returning no error at all — just
// fewer entries than actually exist. Root cause was client.File.readAt
// treating any reply shorter than requested as end-of-file, correct for
// sequential regular-file reads but wrong for directory reads, where
// MarshalDir (this same v0.2.0) almost always returns fewer bytes than
// the offered buffer as a matter of course (whole entries only). Fixed
// upstream in v0.2.1: ReadDirContext now reads directly and stops only on
// a truly empty reply, matching the convention TestDirectoryListingPaginates
// above already exercises directly against dirFile.Read. Kept as a
// standing regression test now that it passes, not just a historical note.
func TestDirectoryListingOverTheWire(t *testing.T) {
	dir := t.TempDir()
	store, err := patches.Open(dir + "/patches")
	if err != nil {
		t.Fatal(err)
	}
	refs, want := manyRefs(t, store, 50)

	fs := &FS{Store: store, Refs: refs}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &server.Server{FS: fs, ConnContext: connContextFor(auth.PermRead)}
	go srv.Serve(ln)

	c, err := client.Dial("tcp", ln.Addr().String(), client.WithMsize(128))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Attach("test", ""); err != nil {
		t.Fatal(err)
	}
	refsFile, err := c.Open("refs", p9.OREAD)
	if err != nil {
		t.Fatal(err)
	}
	defer refsFile.Close()
	stats, err := refsFile.ReadDir()
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, st := range stats {
		got = append(got, st.Name)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(got), len(want), got)
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
	fs := &FS{Store: store, Blobs: blobs, Refs: fakeRefs{}}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &server.Server{FS: fs, ConnContext: connContextFor(auth.PermWrite)}
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
