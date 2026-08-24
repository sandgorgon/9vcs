package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"strings"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"

	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	fingerprint := fs.String("peer-fingerprint", "", "expected fingerprint of the peer being imported from (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fingerprint == "" {
		return fmt.Errorf("import: -peer-fingerprint is required (see `9vcs identity show` on the peer)")
	}
	rest := fs.Args()
	if len(rest) < 2 || len(rest) > 3 {
		return fmt.Errorf("import: expected -peer-fingerprint <hex> <addr> <ref-name> [<local-branch-name>]")
	}
	addr, refName := rest[0], rest[1]
	localName := refName
	if len(rest) == 3 {
		localName = rest[2]
	}

	r, err := findRepo()
	if err != nil {
		return err
	}
	id, err := identity.Load()
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	tlsCfg := id.ClientTLSConfig(func(fp string) bool { return fp == *fingerprint })
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("import: connecting to %s: %w", addr, err)
	}
	defer conn.Close()

	c, err := client.NewClient(conn)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	defer c.Close()
	if _, err := c.Attach("import", ""); err != nil {
		return fmt.Errorf("import: attaching to %s: %w (the peer may have rejected this connection's certificate — check its authorized-peers)", addr, err)
	}

	remoteHash, err := fetchRefHash(c, refName)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	if err := importClosure(c, r.store, r.blobs, remoteHash); err != nil {
		return fmt.Errorf("import: %w", err)
	}

	localHash, exists, err := r.refHash(localName)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if exists {
		if localHash == remoteHash {
			fmt.Println("already up to date")
			return nil
		}
		remoteClosure, err := patches.Closure(r.store, remoteHash)
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}
		if !remoteClosure[localHash] {
			localClosure, err := patches.Closure(r.store, localHash)
			if err != nil {
				return fmt.Errorf("import: %w", err)
			}
			if localClosure[remoteHash] {
				fmt.Printf("local branch %q is already ahead of %s's %q; nothing imported\n", localName, addr, refName)
				return nil
			}
			return fmt.Errorf("import: local branch %q has diverged from %s's %q; reconcile/merge support for this isn't built yet, nothing was changed", localName, addr, refName)
		}
		// remoteClosure[localHash]: local's current history is a prefix of
		// remote's — a fast-forward, safe to just move the ref.
	}

	// If localName is the branch currently checked out, moving its ref
	// without also updating the working tree would desync HEAD from what's
	// actually on disk — checkout/diff would then compare the working tree
	// against the *new* content while the files on disk are still the
	// *old* content, and misreport every line as a local change. Git
	// avoids this by having fetch only ever touch remote-tracking refs,
	// never the checked-out branch directly; this design has no
	// remote-tracking refs, so importing into the checked-out branch has
	// to behave like a fast-forward checkout instead: sync the working
	// tree in the same step, and refuse if that would overwrite
	// uncommitted changes, exactly as checkout's own safety check does.
	branch, err := r.currentBranch()
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if branch == localName {
		oldIdx, err := patches.Materialize(r.store, localHash)
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}
		dirty, err := changedFiles(r, oldIdx)
		if err != nil {
			return err
		}
		if len(dirty) > 0 {
			return fmt.Errorf("import: %q is checked out and has uncommitted changes that would be overwritten; record or discard them first", localName)
		}
		newIdx, err := patches.Materialize(r.store, remoteHash)
		if err != nil {
			return fmt.Errorf("import: %w", err)
		}
		if err := writeWorkingTree(r, oldIdx, newIdx); err != nil {
			return fmt.Errorf("import: writing working tree: %w", err)
		}
	}

	if err := r.setRefHash(localName, remoteHash); err != nil {
		return fmt.Errorf("import: %w", err)
	}
	fmt.Printf("imported %s's %q (%s) into local branch %q\n", addr, refName, remoteHash.String()[:12], localName)
	return nil
}

func fetchRefHash(c *client.Client, name string) (patches.Hash, error) {
	f, err := c.Open("refs/"+name, p9.OREAD)
	if err != nil {
		return patches.Hash{}, fmt.Errorf("fetching ref %q: %w", name, err)
	}
	defer f.Close()
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
