// Package vcsfs exposes a repo's patch/blob/ref storage as a 9P
// server.FileSystem: /patches/<hash>, /blobs/<hash> (both content-addressed;
// immutable once written, but a peer with write permission may create new
// ones — content addressing means a create either matches its claimed hash
// or is rejected, never silently overwrites), and /refs/<name> (the one
// mutable path, CAS-protected on write — see refFile).
package vcsfs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/server"

	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

// RefReader is the subset of a repo's ref storage vcsfs needs to serve
// reads of /refs. Implemented by a small adapter in cmd/9vcs over *repo —
// vcsfs can't import cmd/9vcs itself, that would be backwards.
type RefReader interface {
	RefHash(name string) (patches.Hash, bool, error)
	ListRefs() ([]string, error)
}

// RefWriter is the subset of ref storage vcsfs needs to serve
// CAS-protected writes to /refs.
type RefWriter interface {
	// SetRefHash CAS-updates name's ref to new, succeeding only if its
	// current value is exactly old (the zero hash meaning "must not
	// exist yet"). A stale old — the pusher's view of the ref not
	// matching its actual current value — must fail without effect,
	// not silently overwrite a value the pusher never saw.
	SetRefHash(name string, old, new patches.Hash) error
}

// RefStore is the full ref-storage access vcsfs needs.
type RefStore interface {
	RefReader
	RefWriter
}

// FS is one repo exposed over 9P — a single shared instance serves every
// connection, unlike the per-connection peer permission each of those
// connections carries. That permission reaches Attach via the context, not
// a field on FS: a Server.ConnContext hook authenticates the peer (a TLS
// handshake, an authorized-peers lookup) and calls WithPermission, and
// Attach reads it back with permissionFrom. See PLAN.md's "one deliberate
// deviation" for why per-connection identity has to flow through the
// context this way rather than through FS itself.
type FS struct {
	Store *patches.Store
	Blobs *patches.BlobStore
	Refs  RefStore
}

type permKey struct{}

// WithPermission returns a context carrying perm, for a Server.ConnContext
// hook to attach once it's authenticated the peer — FS.Attach reads it
// back via permissionFrom.
func WithPermission(ctx context.Context, perm identity.Permission) context.Context {
	return context.WithValue(ctx, permKey{}, perm)
}

func permissionFrom(ctx context.Context) (identity.Permission, bool) {
	p, ok := ctx.Value(permKey{}).(identity.Permission)
	return p, ok
}

func (fs *FS) Attach(ctx context.Context, uname, aname string) (server.File, error) {
	perm, ok := permissionFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("vcsfs: no permission in context; the server's ConnContext must call vcsfs.WithPermission")
	}
	return &dirFile{fs: fs, kind: kindRoot, perm: perm}, nil
}

type dirKind int

const (
	kindRoot dirKind = iota
	kindPatches
	kindBlobs
	kindRefs
)

func (k dirKind) name() string {
	switch k {
	case kindPatches:
		return "patches"
	case kindBlobs:
		return "blobs"
	case kindRefs:
		return "refs"
	default:
		return ""
	}
}

type dirFile struct {
	fs   *FS
	kind dirKind
	perm identity.Permission
}

func (d *dirFile) Qid() p9.Qid {
	return p9.Qid{Type: p9.QTDIR, Path: qidPath("dir", d.kind.name())}
}

func (d *dirFile) Stat(ctx context.Context) (p9.Stat, error) {
	name := d.kind.name()
	if name == "" {
		name = "/"
	}
	return p9.Stat{Qid: d.Qid(), Mode: p9.DMDIR | 0o555, Name: name}, nil
}

func (d *dirFile) WStat(ctx context.Context, st p9.Stat) error {
	return fmt.Errorf("vcsfs: %s is read-only", d.kind.name())
}

func (d *dirFile) Walk(ctx context.Context, name string) (server.File, error) {
	switch d.kind {
	case kindRoot:
		switch name {
		case "patches":
			return &dirFile{fs: d.fs, kind: kindPatches, perm: d.perm}, nil
		case "blobs":
			return &dirFile{fs: d.fs, kind: kindBlobs, perm: d.perm}, nil
		case "refs":
			return &dirFile{fs: d.fs, kind: kindRefs, perm: d.perm}, nil
		}
		return nil, fmt.Errorf("vcsfs: no such file %q", name)

	case kindPatches:
		h, err := patches.HashFromHex(name)
		if err != nil {
			return nil, fmt.Errorf("vcsfs: %q is not a valid patch hash", name)
		}
		data, err := d.fs.Store.GetRaw(h)
		if err != nil {
			return nil, fmt.Errorf("vcsfs: patch %s: %w", name, err)
		}
		return &objFile{name: name, data: data, perm: d.perm}, nil

	case kindBlobs:
		h, err := patches.HashFromHex(name)
		if err != nil {
			return nil, fmt.Errorf("vcsfs: %q is not a valid blob hash", name)
		}
		data, err := d.fs.Blobs.Get(h)
		if err != nil {
			return nil, fmt.Errorf("vcsfs: blob %s: %w", name, err)
		}
		return &objFile{name: name, data: data, perm: d.perm}, nil

	case kindRefs:
		h, ok, err := d.fs.Refs.RefHash(name)
		if err != nil {
			return nil, fmt.Errorf("vcsfs: ref %s: %w", name, err)
		}
		if !ok {
			return nil, fmt.Errorf("vcsfs: ref %q not found", name)
		}
		return &refFile{fs: d.fs, name: name, perm: d.perm, exists: true, current: h}, nil
	}
	return nil, fmt.Errorf("vcsfs: no such file %q", name)
}

func (d *dirFile) Open(ctx context.Context, mode p9.Mode) error {
	if mode != p9.OREAD {
		return fmt.Errorf("vcsfs: %s is read-only", d.kind.name())
	}
	return nil
}

// Create makes a new patch, blob, or ref under this directory — the
// write-side counterpart to Walk, used for a name that doesn't exist yet
// (patches.Store and BlobStore are content-addressed, so a name that
// already exists is never legitimately "created" again; a ref that
// already exists is updated via Walk + Open(OWRITE) instead, matching
// normal 9P Tcreate semantics — see refFile).
func (d *dirFile) Create(ctx context.Context, name string, mode9 p9.Mode, openMode p9.Mode) (server.File, error) {
	if d.perm < identity.PermWrite {
		return nil, fmt.Errorf("vcsfs: %s: permission denied", d.kind.name())
	}
	switch d.kind {
	case kindPatches, kindBlobs:
		h, err := patches.HashFromHex(name)
		if err != nil {
			return nil, fmt.Errorf("vcsfs: %q is not a valid hash", name)
		}
		return &writeFile{fs: d.fs, kind: d.kind, want: h}, nil
	case kindRefs:
		if _, ok, err := d.fs.Refs.RefHash(name); err != nil {
			return nil, fmt.Errorf("vcsfs: ref %s: %w", name, err)
		} else if ok {
			return nil, fmt.Errorf("vcsfs: ref %q already exists", name)
		}
		return &refFile{fs: d.fs, name: name, perm: d.perm, exists: false}, nil
	}
	return nil, fmt.Errorf("vcsfs: %s: cannot create here", d.kind.name())
}

// Read lists this directory's children, correctly handling however many
// Read calls it takes to cover the listing (see server.MarshalDir) — not
// just the offset-0 case, which is all a small listing ever exercises but
// isn't the actual contract. Enumerating every object in /patches or
// /blobs isn't implemented at all: content-addressed pull never needs to
// browse them, only fetch a hash it already knows (reached via a ref,
// then Dependencies).
func (d *dirFile) Read(ctx context.Context, offset int64, p []byte) (int, error) {
	var names []string
	switch d.kind {
	case kindRoot:
		names = []string{"patches", "blobs", "refs"}
	case kindRefs:
		var err error
		names, err = d.fs.Refs.ListRefs()
		if err != nil {
			return 0, err
		}
	}
	entries := make([]p9.Stat, len(names))
	for i, name := range names {
		entries[i] = p9.Stat{Qid: p9.Qid{Type: p9.QTFILE}, Mode: 0o444, Name: name}
	}
	return server.MarshalDir(entries, offset, p)
}

func (d *dirFile) Write(ctx context.Context, offset int64, p []byte) (int, error) {
	return 0, fmt.Errorf("vcsfs: %s is read-only", d.kind.name())
}

func (d *dirFile) Remove(ctx context.Context) error {
	return fmt.Errorf("vcsfs: %s is read-only", d.kind.name())
}

func (d *dirFile) Close() error { return nil }

// objFile is a read-only leaf: a patch, a blob, or a ref's hash text.
// Content is loaded once, at Walk time (see dirFile.Walk) — every object
// here is small enough that eager loading is simpler than a lazy Open-time
// fetch, not a performance concern worth the extra state.
type objFile struct {
	name string
	data []byte
	perm identity.Permission
}

func (f *objFile) Qid() p9.Qid {
	return p9.Qid{Type: p9.QTFILE, Path: qidPath("file", f.name)}
}

func (f *objFile) Stat(ctx context.Context) (p9.Stat, error) {
	return p9.Stat{Qid: f.Qid(), Mode: 0o444, Length: uint64(len(f.data)), Name: f.name}, nil
}

func (f *objFile) WStat(ctx context.Context, st p9.Stat) error {
	return fmt.Errorf("vcsfs: %s is read-only", f.name)
}

func (f *objFile) Walk(ctx context.Context, name string) (server.File, error) {
	return nil, fmt.Errorf("vcsfs: %s is not a directory", f.name)
}

func (f *objFile) Open(ctx context.Context, mode p9.Mode) error {
	if mode != p9.OREAD {
		return fmt.Errorf("vcsfs: %s is read-only", f.name)
	}
	return nil
}

func (f *objFile) Create(ctx context.Context, name string, perm p9.Mode, mode p9.Mode) (server.File, error) {
	return nil, fmt.Errorf("vcsfs: %s is read-only", f.name)
}

func (f *objFile) Read(ctx context.Context, offset int64, p []byte) (int, error) {
	if offset >= int64(len(f.data)) {
		return 0, io.EOF
	}
	return copy(p, f.data[offset:]), nil
}

func (f *objFile) Write(ctx context.Context, offset int64, p []byte) (int, error) {
	return 0, fmt.Errorf("vcsfs: %s is read-only", f.name)
}

func (f *objFile) Remove(ctx context.Context) error {
	return fmt.Errorf("vcsfs: %s is read-only", f.name)
}

func (f *objFile) Close() error { return nil }

// writeFile is a new patch or blob being pushed: dirFile.Create returns
// one in place of an error once a caller has write permission and named a
// syntactically valid hash. It accumulates whatever's written across
// however many Write calls the client makes, and finalizes — decoding (for
// a patch) or just storing (for a blob), then verifying the result
// actually hashes to the name it was created under — on Close, the
// natural point a 9P client signals "done," per Tclunk's handling of a
// still-open fid. A mismatch is reported back to the pusher as an error;
// content addressing means there's nothing to "undo" either way, since
// whatever was received simply gets stored under its own real hash.
type writeFile struct {
	fs   *FS
	kind dirKind // kindPatches or kindBlobs
	want patches.Hash

	mu  sync.Mutex
	buf []byte
}

func (f *writeFile) Qid() p9.Qid {
	return p9.Qid{Type: p9.QTFILE, Path: qidPath("write", f.want.String())}
}

func (f *writeFile) Stat(ctx context.Context) (p9.Stat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return p9.Stat{Qid: f.Qid(), Mode: 0o644, Length: uint64(len(f.buf)), Name: f.want.String()}, nil
}

func (f *writeFile) WStat(ctx context.Context, st p9.Stat) error {
	return fmt.Errorf("vcsfs: %s: metadata changes not supported", f.want)
}

func (f *writeFile) Walk(ctx context.Context, name string) (server.File, error) {
	return nil, fmt.Errorf("vcsfs: %s is not a directory", f.want)
}

func (f *writeFile) Open(ctx context.Context, mode p9.Mode) error {
	return fmt.Errorf("vcsfs: %s: already open for creation", f.want)
}

func (f *writeFile) Create(ctx context.Context, name string, perm p9.Mode, mode p9.Mode) (server.File, error) {
	return nil, fmt.Errorf("vcsfs: %s is not a directory", f.want)
}

func (f *writeFile) Read(ctx context.Context, offset int64, p []byte) (int, error) {
	return 0, fmt.Errorf("vcsfs: %s: write-only until created", f.want)
}

func (f *writeFile) Write(ctx context.Context, offset int64, p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	end := offset + int64(len(p))
	if end > int64(len(f.buf)) {
		grown := make([]byte, end)
		copy(grown, f.buf)
		f.buf = grown
	}
	copy(f.buf[offset:], p)
	return len(p), nil
}

func (f *writeFile) Remove(ctx context.Context) error {
	return fmt.Errorf("vcsfs: %s: remove not supported", f.want)
}

// Close finalizes whatever was written — see the writeFile doc comment.
// Nothing was ever written (Create followed immediately by Close, with no
// Write in between) is treated as a benign abort, not an error: there's
// no content to verify or store, so there's nothing to reject either.
func (f *writeFile) Close() error {
	f.mu.Lock()
	data := f.buf
	f.mu.Unlock()
	if len(data) == 0 {
		return nil
	}
	switch f.kind {
	case kindBlobs:
		got, err := f.fs.Blobs.Put(data)
		if err != nil {
			return fmt.Errorf("vcsfs: storing blob: %w", err)
		}
		if got != f.want {
			return fmt.Errorf("vcsfs: written content hashes to %s, not the requested %s", got, f.want)
		}
		return nil
	default: // kindPatches
		p, err := patches.Decode(data)
		if err != nil {
			return fmt.Errorf("vcsfs: invalid patch content for %s: %w", f.want, err)
		}
		// A different check from the got != f.want comparison below:
		// this one is about whether the claimed authorship is genuine,
		// not whether these bytes match the requested hash — a
		// dishonest pusher can craft arbitrary content and correctly
		// self-hash it, but can't produce a valid signature for a
		// fingerprint it doesn't hold the private key for. Checked
		// before Put so a forged authorship claim is never persisted at
		// all. An unsigned patch (no fingerprint claimed) always passes.
		if !p.VerifyAuthorSignature() {
			return fmt.Errorf("vcsfs: patch %s claims authorship by fingerprint %x but its signature doesn't verify — possible forgery", f.want, p.AuthorFingerprint)
		}
		got, err := f.fs.Store.Put(p)
		if err != nil {
			return fmt.Errorf("vcsfs: storing patch: %w", err)
		}
		if got != f.want {
			return fmt.Errorf("vcsfs: written content hashes to %s, not the requested %s", got, f.want)
		}
		return nil
	}
}

// refFile is /refs/<name>, the one mutable path in this filesystem.
// Reached either via dirFile.Walk (an existing ref, for reading or for a
// CAS-protected update) or dirFile.Create (a brand-new one). Content read
// back is a single hex patch hash plus a trailing newline; a write's
// payload is "<expected-old-hash> <new-hash>\n", where expected-old is the
// all-zero hash to mean "must not currently exist" — the pusher's own view
// of what the ref currently is, so a write only takes effect if that view
// is still accurate at the moment of the write. A stale view (someone else
// moved the ref since the pusher last looked) is rejected as a CAS
// conflict rather than silently overwritten; see (*repo).setRefHashCAS in
// cmd/9vcs for where that check actually happens.
type refFile struct {
	fs      *FS
	name    string
	perm    identity.Permission
	exists  bool
	current patches.Hash

	mu  sync.Mutex
	buf []byte
}

func (f *refFile) Qid() p9.Qid { return p9.Qid{Type: p9.QTFILE, Path: qidPath("ref", f.name)} }

func (f *refFile) Stat(ctx context.Context) (p9.Stat, error) {
	length := 0
	if f.exists {
		length = len(f.current.String()) + 1
	}
	return p9.Stat{Qid: f.Qid(), Mode: 0o644, Length: uint64(length), Name: f.name}, nil
}

func (f *refFile) WStat(ctx context.Context, st p9.Stat) error {
	return fmt.Errorf("vcsfs: refs/%s: metadata changes not supported", f.name)
}

func (f *refFile) Walk(ctx context.Context, name string) (server.File, error) {
	return nil, fmt.Errorf("vcsfs: refs/%s is not a directory", f.name)
}

func (f *refFile) Open(ctx context.Context, mode p9.Mode) error {
	switch mode {
	case p9.OREAD:
		if !f.exists {
			return fmt.Errorf("vcsfs: ref %q not found", f.name)
		}
		return nil
	case p9.OWRITE:
		if f.perm < identity.PermWrite {
			return fmt.Errorf("vcsfs: refs/%s: permission denied", f.name)
		}
		return nil
	default:
		return fmt.Errorf("vcsfs: refs/%s: unsupported open mode", f.name)
	}
}

func (f *refFile) Create(ctx context.Context, name string, perm p9.Mode, mode p9.Mode) (server.File, error) {
	return nil, fmt.Errorf("vcsfs: refs/%s is not a directory", f.name)
}

func (f *refFile) Read(ctx context.Context, offset int64, p []byte) (int, error) {
	if !f.exists {
		return 0, fmt.Errorf("vcsfs: ref %q not found", f.name)
	}
	data := []byte(f.current.String() + "\n")
	if offset >= int64(len(data)) {
		return 0, io.EOF
	}
	return copy(p, data[offset:]), nil
}

func (f *refFile) Write(ctx context.Context, offset int64, p []byte) (int, error) {
	if f.perm < identity.PermWrite {
		return 0, fmt.Errorf("vcsfs: refs/%s: permission denied", f.name)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	end := offset + int64(len(p))
	if end > int64(len(f.buf)) {
		grown := make([]byte, end)
		copy(grown, f.buf)
		f.buf = grown
	}
	copy(f.buf[offset:], p)
	return len(p), nil
}

func (f *refFile) Remove(ctx context.Context) error {
	return fmt.Errorf("vcsfs: refs/%s: remove not supported", f.name)
}

// Close parses the CAS payload and applies it — see the refFile doc
// comment. As with writeFile, an Open followed by Close with no Write in
// between (a pure read, or an abandoned write) is a benign no-op, not an
// error.
func (f *refFile) Close() error {
	f.mu.Lock()
	payload := f.buf
	f.buf = nil
	f.mu.Unlock()
	if len(payload) == 0 {
		return nil
	}
	fields := strings.Fields(strings.TrimSpace(string(payload)))
	if len(fields) != 2 {
		return fmt.Errorf("vcsfs: refs/%s: write payload must be \"<expected-old-hash> <new-hash>\", got %q", f.name, payload)
	}
	oldHash, err := patches.HashFromHex(fields[0])
	if err != nil {
		return fmt.Errorf("vcsfs: refs/%s: expected-old %q: %w", f.name, fields[0], err)
	}
	newHash, err := patches.HashFromHex(fields[1])
	if err != nil {
		return fmt.Errorf("vcsfs: refs/%s: new %q: %w", f.name, fields[1], err)
	}
	if err := f.fs.Refs.SetRefHash(f.name, oldHash, newHash); err != nil {
		return fmt.Errorf("vcsfs: refs/%s: %w", f.name, err)
	}
	return nil
}

func qidPath(kind, name string) uint64 {
	sum := sha256.Sum256([]byte(kind + "/" + name))
	return binary.BigEndian.Uint64(sum[:8])
}
