// Package vcsfs exposes a repo's patch/blob/ref storage as a 9P
// server.FileSystem: /patches/<hash>, /blobs/<hash> (both content-addressed,
// immutable, read-only), and /refs/<name> (read-only for now — write
// support is a reconcile concern, not needed for a one-directional import).
package vcsfs

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/server"

	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

// RefReader is the subset of a repo's ref storage vcsfs needs to serve
// /refs. Implemented by a small adapter in cmd/9vcs over *repo — vcsfs
// can't import cmd/9vcs itself, that would be backwards.
type RefReader interface {
	RefHash(name string) (patches.Hash, bool, error)
	ListRefs() ([]string, error)
}

// FS is one repo exposed over 9P. One FS is constructed per accepted
// connection, carrying that peer's already-verified permission — see
// PLAN.md's "one deliberate deviation": cmd/9vcs's serve command runs its
// own accept loop instead of server.Serve, specifically so it can extract
// the verified peer fingerprint and look up its permission before
// constructing the FS that connection gets.
type FS struct {
	Store *patches.Store
	Blobs *patches.BlobStore
	Refs  RefReader
	Perm  identity.Permission
}

func (fs *FS) Attach(ctx context.Context, uname, aname string) (server.File, error) {
	return &dirFile{fs: fs, kind: kindRoot}, nil
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
			return &dirFile{fs: d.fs, kind: kindPatches}, nil
		case "blobs":
			return &dirFile{fs: d.fs, kind: kindBlobs}, nil
		case "refs":
			return &dirFile{fs: d.fs, kind: kindRefs}, nil
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
		return &objFile{name: name, data: data}, nil

	case kindBlobs:
		h, err := patches.HashFromHex(name)
		if err != nil {
			return nil, fmt.Errorf("vcsfs: %q is not a valid blob hash", name)
		}
		data, err := d.fs.Blobs.Get(h)
		if err != nil {
			return nil, fmt.Errorf("vcsfs: blob %s: %w", name, err)
		}
		return &objFile{name: name, data: data}, nil

	case kindRefs:
		h, ok, err := d.fs.Refs.RefHash(name)
		if err != nil {
			return nil, fmt.Errorf("vcsfs: ref %s: %w", name, err)
		}
		if !ok {
			return nil, fmt.Errorf("vcsfs: ref %q not found", name)
		}
		return &objFile{name: name, data: []byte(h.String() + "\n")}, nil
	}
	return nil, fmt.Errorf("vcsfs: no such file %q", name)
}

func (d *dirFile) Open(ctx context.Context, mode p9.Mode) error {
	if mode != p9.OREAD {
		return fmt.Errorf("vcsfs: %s is read-only", d.kind.name())
	}
	return nil
}

func (d *dirFile) Create(ctx context.Context, name string, perm p9.Mode, mode p9.Mode) (server.File, error) {
	return nil, fmt.Errorf("vcsfs: %s is read-only", d.kind.name())
}

// Read lists this directory's children as concatenated Stat blobs, per
// 9P's directory-read convention. No offset pagination — every listing
// here is small enough that one Read from offset 0 covers it, a
// deliberate simplification for what this scaffold actually needs.
// Enumerating every object in /patches or /blobs isn't implemented at
// all: content-addressed pull never needs to browse them, only fetch a
// hash it already knows (reached via a ref, then Dependencies).
func (d *dirFile) Read(ctx context.Context, offset int64, p []byte) (int, error) {
	if offset > 0 {
		return 0, io.EOF
	}
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
	var buf []byte
	for _, name := range names {
		st := p9.Stat{Qid: p9.Qid{Type: p9.QTFILE}, Mode: 0o444, Name: name}
		buf = append(buf, st.Marshal()...)
	}
	return copy(p, buf), nil
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

func qidPath(kind, name string) uint64 {
	sum := sha256.Sum256([]byte(kind + "/" + name))
	return binary.BigEndian.Uint64(sum[:8])
}
