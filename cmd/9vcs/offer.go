package main

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"io"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"

	"github.com/sandgorgon/9auth"
	"github.com/sandgorgon/9vcs/bundle"
	"github.com/sandgorgon/9vcs/objstore/patches"
)

// cmdOffer dispatches on args[0] the same style cmdBundle already uses,
// with posting as the unlabeled default case — see PLAN.md decision #8's
// "/offers live variant — concrete scope". An offer is a bundle.Export'd
// payload (see cmdBundlePost below) pushed to a live peer's /offers
// instead of written to a file; list/apply/remove are the maintainer-side
// counterparts to bundle show/import plus the one thing bundle files
// don't need at all, cleanup of a handled entry.
func cmdOffer(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("offer: usage: 9vcs offer [-m MSG] [-peer-fingerprint FP] <peer-addr> <ref-or-hash>...  |  9vcs offer list <peer-addr>  |  9vcs offer apply <peer-addr> <offer-id>  |  9vcs offer remove <peer-addr> <offer-id>")
	}
	switch args[0] {
	case "list":
		return cmdOfferList(args[1:])
	case "apply":
		return cmdOfferApply(args[1:])
	case "remove":
		return cmdOfferRemove(args[1:])
	default:
		return cmdOfferPost(args)
	}
}

func cmdOfferPost(args []string) error {
	fs := flag.NewFlagSet("offer", flag.ExitOnError)
	message := fs.String("m", "", "offer message")
	fingerprint := fs.String("peer-fingerprint", "", "expected fingerprint of the peer, as an explicit one-off pin; omit to use the known-peers store, prompting on first connection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 2 {
		return fmt.Errorf("offer: usage: 9vcs offer [-m MSG] [-peer-fingerprint FP] <peer-addr> <ref-or-hash>...")
	}
	addr, refArgs := rest[0], rest[1:]

	r, err := findRepo()
	if err != nil {
		return err
	}
	roots := make([]patches.Hash, 0, len(refArgs))
	for _, arg := range refArgs {
		h, err := r.resolveRef(arg)
		if err != nil {
			return fmt.Errorf("offer: %w", err)
		}
		roots = append(roots, h)
	}

	id, err := auth.Load()
	if err != nil {
		return fmt.Errorf("offer: loading identity: %w", err)
	}
	data, n, err := bundle.Export(r.store, r.blobs, roots, *message, id.Key)
	if err != nil {
		return fmt.Errorf("offer: %w", err)
	}

	c, _, err := dialPeer(*fingerprint, addr)
	if err != nil {
		return fmt.Errorf("offer: %w", err)
	}
	defer c.Close()

	offerID, err := pushOffer(c, data)
	if err != nil {
		return fmt.Errorf("offer: %w", err)
	}
	fmt.Printf("posted %d patch(es) to %s's offers as %s, signed as %s\n", n, addr, offerID, id.Fingerprint())
	return nil
}

// pushOffer creates the offer on the peer under its own content hash — the
// same "claim a hash, let the peer's Close verify it" shape pushPatch and
// pushBlobIfMissing (reconcile.go) already use for /patches and /blobs.
func pushOffer(c *client.Client, data []byte) (patches.Hash, error) {
	id := patches.Hash(sha256.Sum256(data))
	f, err := c.Create("offers/"+id.String(), 0o644, p9.OWRITE)
	if err != nil {
		return patches.Hash{}, fmt.Errorf("creating offer on peer: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return patches.Hash{}, fmt.Errorf("writing offer to peer: %w", err)
	}
	if err := f.Close(); err != nil {
		return patches.Hash{}, fmt.Errorf("offer rejected by peer: %w", err)
	}
	return id, nil
}

func cmdOfferList(args []string) error {
	fs := flag.NewFlagSet("offer list", flag.ExitOnError)
	fingerprint := fs.String("peer-fingerprint", "", "expected fingerprint of the peer, as an explicit one-off pin; omit to use the known-peers store, prompting on first connection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("offer list: usage: 9vcs offer list [-peer-fingerprint FP] <peer-addr>")
	}
	addr := rest[0]

	c, _, err := dialPeer(*fingerprint, addr)
	if err != nil {
		return fmt.Errorf("offer list: %w", err)
	}
	defer c.Close()

	dir, err := c.Open("offers", p9.OREAD)
	if err != nil {
		return fmt.Errorf("offer list: opening %s's offers: %w", addr, err)
	}
	defer dir.Close()
	stats, err := dir.ReadDir()
	if err != nil {
		return fmt.Errorf("offer list: %w", err)
	}
	if len(stats) == 0 {
		fmt.Println("no pending offers")
		return nil
	}
	for _, st := range stats {
		b, err := fetchOffer(c, st.Name)
		if err != nil {
			fmt.Printf("%s: %v\n", st.Name, err)
			continue
		}
		status := "verified"
		if !b.Verify() {
			status = "INVALID SIGNATURE"
		}
		fmt.Printf("%s  signer %s (%s)  %d patch(es)  %s\n", st.Name, auth.Fingerprint(b.SignerPub), status, len(b.Patches), b.Message)
	}
	return nil
}

func cmdOfferApply(args []string) error {
	fs := flag.NewFlagSet("offer apply", flag.ExitOnError)
	fingerprint := fs.String("peer-fingerprint", "", "expected fingerprint of the peer, as an explicit one-off pin; omit to use the known-peers store, prompting on first connection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return fmt.Errorf("offer apply: usage: 9vcs offer apply [-peer-fingerprint FP] <peer-addr> <offer-id>")
	}
	addr, offerID := rest[0], rest[1]

	r, err := findRepo()
	if err != nil {
		return err
	}
	c, _, err := dialPeer(*fingerprint, addr)
	if err != nil {
		return fmt.Errorf("offer apply: %w", err)
	}
	defer c.Close()

	b, err := fetchOffer(c, offerID)
	if err != nil {
		return fmt.Errorf("offer apply: %w", err)
	}
	if !b.Verify() {
		return fmt.Errorf("offer apply: %s has an invalid signature — corrupted or tampered with", offerID)
	}
	if err := b.Store(r.store, r.blobs); err != nil {
		return fmt.Errorf("offer apply: %w", err)
	}
	printBundleSummary(b)
	fmt.Println("\nnothing integrated yet — inspect with `9vcs diff <hash>`, then `9vcs apply <hash>` to bring it in")
	return nil
}

func cmdOfferRemove(args []string) error {
	fs := flag.NewFlagSet("offer remove", flag.ExitOnError)
	fingerprint := fs.String("peer-fingerprint", "", "expected fingerprint of the peer, as an explicit one-off pin; omit to use the known-peers store, prompting on first connection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return fmt.Errorf("offer remove: usage: 9vcs offer remove [-peer-fingerprint FP] <peer-addr> <offer-id>")
	}
	addr, offerID := rest[0], rest[1]

	c, _, err := dialPeer(*fingerprint, addr)
	if err != nil {
		return fmt.Errorf("offer remove: %w", err)
	}
	defer c.Close()

	root, err := c.Attach("client", "")
	if err != nil {
		return fmt.Errorf("offer remove: attaching to %s: %w", addr, err)
	}
	offerFid, err := root.Walk("offers", offerID)
	if err != nil {
		return fmt.Errorf("offer remove: %s not found on %s: %w", offerID, addr, err)
	}
	if err := offerFid.Remove(); err != nil {
		return fmt.Errorf("offer remove: %w", err)
	}
	fmt.Printf("removed offer %s from %s\n", offerID, addr)
	return nil
}

// fetchOffer reads and decodes (but does not store or verify) the offer
// named id on the peer — shared by list (pure inspection) and apply
// (which verifies and stores after this returns).
func fetchOffer(c *client.Client, id string) (*bundle.Bundle, error) {
	f, err := c.Open("offers/"+id, p9.OREAD)
	if err != nil {
		return nil, fmt.Errorf("fetching offer %s: %w", id, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("reading offer %s: %w", id, err)
	}
	b, err := bundle.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("decoding offer %s: %w", id, err)
	}
	return b, nil
}
