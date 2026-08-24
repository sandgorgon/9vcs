package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"strings"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"

	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

// dialPeer connects to addr over TLS, verifying it presents fingerprint
// (the pin the caller was given, e.g. via -peer-fingerprint), and attaches
// to its root. Shared by import and reconcile — both start a connection
// exactly the same way.
func dialPeer(fingerprint, addr string) (*client.Client, error) {
	id, err := identity.Load()
	if err != nil {
		return nil, err
	}
	tlsCfg := id.ClientTLSConfig(func(fp string) bool { return fp == fingerprint })
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", addr, err)
	}
	c, err := client.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := c.Attach("client", ""); err != nil {
		c.Close()
		return nil, fmt.Errorf("attaching to %s: %w (the peer may have rejected this connection's certificate — check its authorized-peers)", addr, err)
	}
	return c, nil
}

// syncDirection is what a local ref and a peer's ref need, relative to
// each other, to reconcile — see classify.
type syncDirection int

const (
	dirUpToDate syncDirection = iota
	dirPull                   // remote is ahead of local (or local doesn't exist yet)
	dirPush                   // local is ahead of remote (or remote doesn't exist yet)
	dirDiverged
)

// classify compares localHash and remoteHash — both already fully present
// locally; see importClosure — and reports which direction, if any, would
// safely reconcile them. Absent refs are represented by the zero hash on
// either side; patches.Closure never contains the zero hash itself (it's
// a sentinel, not a real dependency), so the zero-hash cases need their
// own branches rather than falling out of the general closure-membership
// check below.
func classify(r *repo, localHash, remoteHash patches.Hash) (syncDirection, error) {
	switch {
	case localHash == remoteHash:
		return dirUpToDate, nil
	case localHash.IsZero():
		return dirPull, nil
	case remoteHash.IsZero():
		return dirPush, nil
	}
	remoteClosure, err := patches.Closure(r.store, remoteHash)
	if err != nil {
		return 0, err
	}
	if remoteClosure[localHash] {
		return dirPull, nil
	}
	localClosure, err := patches.Closure(r.store, localHash)
	if err != nil {
		return 0, err
	}
	if localClosure[remoteHash] {
		return dirPush, nil
	}
	return dirDiverged, nil
}

// pullRef finishes a fast-forward pull once the caller has already
// decided remoteHash is safe to adopt for localName (see classify) and
// its full dependency closure is already present locally (see
// importClosure): syncs the working tree too if localName happens to be
// the branch currently checked out, then moves the local ref.
//
// If localName is the checked-out branch, moving its ref without also
// updating the working tree would desync HEAD from what's actually on
// disk — checkout/diff would then compare the working tree against the
// *new* content while the files on disk are still the *old* content, and
// misreport every line as a local change. Git avoids this by having
// fetch only ever touch remote-tracking refs, never the checked-out
// branch directly; this design has no remote-tracking refs, so pulling
// into the checked-out branch has to behave like a fast-forward checkout
// instead — refusing if that would overwrite uncommitted changes, exactly
// as checkout's own safety check does.
func pullRef(r *repo, localName string, localHash, remoteHash patches.Hash) error {
	branch, err := r.currentBranch()
	if err != nil {
		return err
	}
	if branch == localName {
		oldIdx, err := patches.Materialize(r.store, localHash)
		if err != nil {
			return err
		}
		dirty, err := changedFiles(r, oldIdx)
		if err != nil {
			return err
		}
		if len(dirty) > 0 {
			return fmt.Errorf("%q is checked out and has uncommitted changes that would be overwritten; record or discard them first", localName)
		}
		newIdx, err := patches.Materialize(r.store, remoteHash)
		if err != nil {
			return err
		}
		if err := writeWorkingTree(r, oldIdx, newIdx); err != nil {
			return fmt.Errorf("writing working tree: %w", err)
		}
	}
	return r.setRefHash(localName, remoteHash)
}

// fetchRefHash fetches a peer's ref, treating its absence as an error —
// what import wants: the user named a specific ref they expect to exist.
func fetchRefHash(c *client.Client, name string) (patches.Hash, error) {
	f, err := c.Open("refs/"+name, p9.OREAD)
	if err != nil {
		return patches.Hash{}, fmt.Errorf("fetching ref %q: %w", name, err)
	}
	defer f.Close()
	return decodeRefFile(f, name)
}

// fetchRefHashOrZero is fetchRefHash but treats any failure to open the
// ref as "doesn't exist yet" (the zero hash) rather than an error — what
// reconcile wants: the peer legitimately may not have this branch yet,
// which just means the right direction is push, not a failure. Base
// 9P2000 has no structured error codes to distinguish "not found" from
// other failures (see PLAN.md's library facts), so this is a deliberate
// judgment call, not a precise check — a real permission or network
// problem surfaces anyway, at the closure-fetch or push step that
// follows.
func fetchRefHashOrZero(c *client.Client, name string) (patches.Hash, error) {
	f, err := c.Open("refs/"+name, p9.OREAD)
	if err != nil {
		return patches.Hash{}, nil
	}
	defer f.Close()
	return decodeRefFile(f, name)
}

func decodeRefFile(f *client.File, name string) (patches.Hash, error) {
	data, err := io.ReadAll(f)
	if err != nil {
		return patches.Hash{}, fmt.Errorf("reading ref %q: %w", name, err)
	}
	h, err := patches.HashFromHex(strings.TrimSpace(string(data)))
	if err != nil {
		return patches.Hash{}, fmt.Errorf("peer's ref %q: %w", name, err)
	}
	return h, nil
}

// importClosure fetches root and everything it transitively depends on
// (patches, and any blobs a fetched patch's KindBlob changes reference)
// that isn't already present locally, verifying each object's content
// actually hashes to the hash it was requested by before storing it.
// Content addressing is what makes this a plain recursive pull with no
// separate have/want negotiation: "do I have this hash" is the only
// question that ever needs asking, and Store.Has answers it locally.
func importClosure(c *client.Client, store *patches.Store, blobs *patches.BlobStore, root patches.Hash) error {
	seen := map[patches.Hash]bool{}
	var walk func(h patches.Hash) error
	walk = func(h patches.Hash) error {
		if h.IsZero() || seen[h] {
			return nil
		}
		seen[h] = true
		if store.Has(h) {
			return nil // and transitively everything it needs too, by induction
		}
		p, err := fetchPatch(c, store, h)
		if err != nil {
			return err
		}
		for _, dep := range p.Dependencies {
			if err := walk(dep); err != nil {
				return err
			}
		}
		for _, fc := range p.Changes {
			if fc.Kind == patches.KindBlob {
				if err := fetchBlob(c, blobs, fc.Blob); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(root)
}

func fetchPatch(c *client.Client, store *patches.Store, hash patches.Hash) (*patches.Patch, error) {
	f, err := c.Open("patches/"+hash.String(), p9.OREAD)
	if err != nil {
		return nil, fmt.Errorf("fetching patch %s: %w", hash, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading patch %s: %w", hash, err)
	}
	p, err := patches.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decoding patch %s: %w", hash, err)
	}
	if p.Hash() != hash {
		return nil, fmt.Errorf("patch %s: received content hashes to %s instead — corrupted or untrustworthy transfer", hash, p.Hash())
	}
	if _, err := store.Put(p); err != nil {
		return nil, fmt.Errorf("storing patch %s: %w", hash, err)
	}
	return p, nil
}

func fetchBlob(c *client.Client, blobs *patches.BlobStore, hash patches.Hash) error {
	if blobs.Has(hash) {
		return nil
	}
	f, err := c.Open("blobs/"+hash.String(), p9.OREAD)
	if err != nil {
		return fmt.Errorf("fetching blob %s: %w", hash, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("reading blob %s: %w", hash, err)
	}
	got, err := blobs.Put(data)
	if err != nil {
		return fmt.Errorf("storing blob %s: %w", hash, err)
	}
	if got != hash {
		return fmt.Errorf("blob %s: received content hashes to %s instead — corrupted or untrustworthy transfer", hash, got)
	}
	return nil
}
