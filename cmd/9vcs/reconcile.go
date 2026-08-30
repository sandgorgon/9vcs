package main

import (
	"flag"
	"fmt"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"

	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

// cmdReconcile syncs one ref with a peer in whichever direction is safe:
// pull if the peer is ahead, push if we are, or — on genuine divergence —
// fetch what's missing and defer to a local `merge` rather than trying to
// resolve a conflict over the wire (there's no working tree or human on
// the other end of a reconcile to resolve one against). See PLAN.md's
// "reconcile protocol" open item for why this is scoped the way it is.
func cmdReconcile(args []string) error {
	fs := flag.NewFlagSet("reconcile", flag.ExitOnError)
	fingerprint := fs.String("peer-fingerprint", "", "expected fingerprint of the peer, as an explicit one-off pin; omit to use the known-peers store, prompting on first connection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 || len(rest) > 3 {
		return fmt.Errorf("reconcile: expected -peer-fingerprint <hex> <addr> <ref-name> [<local-branch-name>]")
	}
	addr, refName := rest[0], rest[1]
	localName := refName
	if len(rest) == 3 {
		localName = rest[2]
	}

	r, err := repo.Find()
	if err != nil {
		return err
	}
	c, peerFingerprint, err := dialPeer(*fingerprint, addr)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	defer c.Close()

	remoteHash, err := fetchRefHashOrZero(c, refName)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	localHash, _, err := r.RefHash(localName)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	if localHash.IsZero() && remoteHash.IsZero() {
		fmt.Println("nothing to reconcile")
		return nil
	}

	// Fetch whatever the peer has that we don't, unconditionally: needed
	// to determine ancestry regardless of which direction turns out to be
	// right, and a no-op (Store.Has skips already-present objects) if
	// there's nothing new — including the "we're actually ahead" case,
	// where this just confirms it cheaply rather than transferring
	// anything.
	stats, err := importClosure(c, r.Store, r.Blobs, remoteHash, peerFingerprint)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	printImportStats(stats, peerFingerprint)

	dir, err := classify(r, localHash, remoteHash)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	switch dir {
	case dirUpToDate:
		fmt.Println("already up to date")
		return nil
	case dirPull:
		if err := pullRef(r, localName, localHash, remoteHash); err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
		fmt.Printf("pulled %s's %q (%s) into local branch %q\n", addr, refName, remoteHash.String()[:12], localName)
		return nil
	case dirPush:
		if err := pushRef(c, r.Store, r.Blobs, refName, remoteHash, localHash); err != nil {
			return fmt.Errorf("reconcile: %w", err)
		}
		fmt.Printf("pushed local branch %q (%s) to %s's %q\n", localName, localHash.String()[:12], addr, refName)
		return nil
	default: // dirDiverged
		fmt.Printf("local branch %q and %s's %q have diverged; fetched the missing history — check out %q, run `9vcs merge %s`, then reconcile again to push the merged result\n", localName, addr, refName, localName, remoteHash.String()[:12])
		return nil
	}
}

// pushRef sends everything localHash's history depends on that the peer
// is missing (see pushClosure), then CAS-updates the peer's ref from
// remoteHash — the value this process observed — to localHash. A stale
// remoteHash (the peer's ref moved since it was fetched, e.g. a
// concurrent push from someone else) is rejected server-side by the same
// CAS check vcsfs.refFile.Close runs locally, and reaches us as Close's
// returned error (github.com/sandgorgon/9p v0.4.0+; earlier versions
// discarded it — see CHANGELOG.md's v0.4.0 entry).
func pushRef(c *client.Client, store *patches.Store, blobs *patches.BlobStore, refName string, remoteHash, localHash patches.Hash) error {
	if err := pushClosure(c, store, blobs, localHash); err != nil {
		return err
	}

	var f *client.File
	var err error
	if remoteHash.IsZero() {
		f, err = c.Create("refs/"+refName, 0o644, p9.OWRITE)
	} else {
		f, err = c.Open("refs/"+refName, p9.OWRITE)
	}
	if err != nil {
		return fmt.Errorf("opening peer's ref %q for write: %w", refName, err)
	}
	payload := fmt.Sprintf("%s %s\n", remoteHash, localHash)
	if _, err := f.Write([]byte(payload)); err != nil {
		f.Close()
		return fmt.Errorf("writing peer's ref %q: %w", refName, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("push of %q rejected: %w", refName, err)
	}
	return nil
}

// pushClosure sends root and everything it transitively depends on
// (patches, and any blobs a sent patch's KindBlob changes reference) that
// the peer doesn't already have — the mirror image of importClosure,
// existence-checked remotely (via a Walk/Open probe) instead of locally
// (via Store.Has).
func pushClosure(c *client.Client, store *patches.Store, blobs *patches.BlobStore, root patches.Hash) error {
	seen := map[patches.Hash]bool{}
	var walk func(h patches.Hash) error
	walk = func(h patches.Hash) error {
		if h.IsZero() || seen[h] {
			return nil
		}
		seen[h] = true
		exists, err := remoteHasPatch(c, h)
		if err != nil {
			return err
		}
		if exists {
			return nil // and transitively everything it needs too, by induction
		}
		p, err := store.Get(h)
		if err != nil {
			return fmt.Errorf("reading local patch %s: %w", h, err)
		}
		for _, dep := range p.Dependencies {
			if err := walk(dep); err != nil {
				return err
			}
		}
		for _, fc := range p.Changes {
			if fc.Kind == patches.KindBlob {
				if err := pushBlobIfMissing(c, blobs, fc.Blob); err != nil {
					return err
				}
			}
		}
		return pushPatch(c, store, h)
	}
	return walk(root)
}

func remoteHasPatch(c *client.Client, h patches.Hash) (bool, error) {
	f, err := c.Open("patches/"+h.String(), p9.OREAD)
	if err != nil {
		return false, nil
	}
	f.Close()
	return true, nil
}

// pushPatch creates hash on the peer and writes its exact stored bytes —
// see Store.GetRaw. A hash mismatch (the bytes don't actually hash to
// hash) is rejected by the peer's vcsfs.writeFile.Close and reaches us
// as Close's returned error — see pushRef's doc comment.
func pushPatch(c *client.Client, store *patches.Store, hash patches.Hash) error {
	data, err := store.GetRaw(hash)
	if err != nil {
		return fmt.Errorf("reading local patch %s: %w", hash, err)
	}
	f, err := c.Create("patches/"+hash.String(), 0o644, p9.OWRITE)
	if err != nil {
		return fmt.Errorf("creating patch %s on peer: %w", hash, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("writing patch %s to peer: %w", hash, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("push of patch %s rejected by peer: %w", hash, err)
	}
	return nil
}

func pushBlobIfMissing(c *client.Client, blobs *patches.BlobStore, hash patches.Hash) error {
	if f, err := c.Open("blobs/"+hash.String(), p9.OREAD); err == nil {
		f.Close()
		return nil
	}
	data, err := blobs.Get(hash)
	if err != nil {
		return fmt.Errorf("reading local blob %s: %w", hash, err)
	}
	wf, err := c.Create("blobs/"+hash.String(), 0o644, p9.OWRITE)
	if err != nil {
		return fmt.Errorf("creating blob %s on peer: %w", hash, err)
	}
	if _, err := wf.Write(data); err != nil {
		wf.Close()
		return fmt.Errorf("writing blob %s to peer: %w", hash, err)
	}
	if err := wf.Close(); err != nil {
		return fmt.Errorf("push of blob %s rejected by peer: %w", hash, err)
	}
	return nil
}
