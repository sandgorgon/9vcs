package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"strings"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"

	"github.com/sandgorgon/9auth"
	"github.com/sandgorgon/9vcs/objstore/patches"
	"github.com/sandgorgon/9vcs/repo"
)

// dialPeer connects to addr over TLS and attaches to its root, returning
// the peer's verified fingerprint alongside the client — import/reconcile
// use it to cross-check a fetched patch's own AuthorFingerprint (see
// importClosure). Shared by import and reconcile — both start a
// connection exactly the same way.
//
// pinnedFingerprint, if non-empty (the caller passed -peer-fingerprint),
// is an explicit one-off pin: the peer must present exactly that
// fingerprint. Otherwise the connection is verified against this
// install's known-peers store (auth.KnownPeers): a known address
// must match its recorded fingerprint exactly, and a genuinely new
// address triggers an interactive first-connect trust prompt on stderr —
// PLAN.md's "known-peers store with TOFU semantics" (known_hosts
// behavior). Either a successful pin or a successful first-connect
// prompt is remembered in known-peers, so later calls don't need either.
func dialPeer(pinnedFingerprint, addr string) (*client.Client, string, error) {
	id, err := auth.Load()
	if err != nil {
		return nil, "", err
	}
	knownPeersPath, err := auth.KnownPeersPath()
	if err != nil {
		return nil, "", err
	}
	known, err := auth.LoadKnownPeers(knownPeersPath)
	if err != nil {
		return nil, "", err
	}

	// tls.Config.VerifyPeerCertificate's callback only reports accept/
	// refuse as a bool; verifyErr carries the specific reason back out so
	// the caller sees more than TLS's generic handshake failure.
	// peerFingerprint captures what was actually verified, for the
	// caller to use once the handshake succeeds.
	var verifyErr error
	var peerFingerprint string
	accept := func(presented string) bool {
		if err := verifyPeer(knownPeersPath, known, addr, pinnedFingerprint, presented, os.Stdin); err != nil {
			verifyErr = err
			return false
		}
		peerFingerprint = presented
		return true
	}

	tlsCfg := id.ClientTLSConfig(accept)
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		if verifyErr != nil {
			return nil, "", verifyErr
		}
		return nil, "", fmt.Errorf("connecting to %s: %w", addr, err)
	}
	c, err := client.NewClient(conn)
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	if _, err := c.Attach("client", ""); err != nil {
		c.Close()
		return nil, "", fmt.Errorf("attaching to %s: %w (the peer may have rejected this connection's certificate — check its authorized-peers)", addr, err)
	}
	return c, peerFingerprint, nil
}

// verifyPeer decides whether to trust addr's presented fingerprint,
// consulting an explicit pin first, then the known-peers store, then —
// on a genuine first connection — prompting on prompt (os.Stdin in
// normal use; a fixed reader in tests). A mismatch against either the
// pin or an existing known-peers entry is always a refusal, never a
// prompt: a changed fingerprint for an address already vouched for is
// exactly what TOFU exists to catch loudly, not paper over.
func verifyPeer(knownPeersPath string, known auth.KnownPeers, addr, pinned, presented string, prompt io.Reader) error {
	if pinned != "" {
		if presented != pinned {
			return fmt.Errorf("peer %s presented fingerprint %s, not the pinned %s", addr, presented, pinned)
		}
		return auth.RememberPeer(knownPeersPath, addr, presented)
	}
	if fp, ok := known[addr]; ok {
		if presented != fp {
			return fmt.Errorf("REMOTE FINGERPRINT HAS CHANGED for %s\n  known:     %s\n  presented: %s\nsomeone may be impersonating this peer, or it legitimately regenerated its identity — if you're sure it's legitimate, re-run with -peer-fingerprint %s to re-pin it in %s", addr, fp, presented, presented, knownPeersPath)
		}
		return nil
	}
	trust, err := promptTrustPeer(prompt, addr, presented)
	if err != nil {
		return fmt.Errorf("reading trust prompt response: %w", err)
	}
	if !trust {
		return fmt.Errorf("connection to %s declined: fingerprint %s not trusted", addr, presented)
	}
	return auth.RememberPeer(knownPeersPath, addr, presented)
}

// promptTrustPeer is the "first-connect prompt" PLAN.md's TOFU decision
// calls for: shown once per genuinely new address, never for one already
// in known-peers (that path is either a silent match or a loud refusal —
// see verifyPeer).
func promptTrustPeer(in io.Reader, addr, fingerprint string) (bool, error) {
	fmt.Fprintf(os.Stderr, "The authenticity of peer %q can't be established.\nFingerprint: %s\n", addr, fingerprint)
	fmt.Fprint(os.Stderr, "Trust this peer and remember it for future connections? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
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
func classify(r *repo.Repo, localHash, remoteHash patches.Hash) (syncDirection, error) {
	switch {
	case localHash == remoteHash:
		return dirUpToDate, nil
	case localHash.IsZero():
		return dirPull, nil
	case remoteHash.IsZero():
		return dirPush, nil
	}
	remoteClosure, err := patches.Closure(r.Store, remoteHash)
	if err != nil {
		return 0, err
	}
	if remoteClosure[localHash] {
		return dirPull, nil
	}
	localClosure, err := patches.Closure(r.Store, localHash)
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
func pullRef(r *repo.Repo, localName string, localHash, remoteHash patches.Hash) error {
	branch, err := r.CurrentBranch()
	if err != nil {
		return err
	}
	if branch == localName {
		oldIdx, err := r.Materialize(localHash)
		if err != nil {
			return err
		}
		dirty, err := repo.ChangedFiles(r, oldIdx)
		if err != nil {
			return err
		}
		if len(dirty) > 0 {
			return fmt.Errorf("%q is checked out and has uncommitted changes that would be overwritten; record or discard them first", localName)
		}
		newIdx, err := r.Materialize(remoteHash)
		if err != nil {
			return err
		}
		if err := repo.WriteWorkingTree(r, oldIdx, newIdx); err != nil {
			return fmt.Errorf("writing working tree: %w", err)
		}
	}
	return r.SetLocalRefCAS(localName, localHash, remoteHash)
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

// importStats summarizes what importClosure actually fetched, including
// the AuthorFingerprint-vs-peer-connection cross-check PLAN.md's Author
// identity subsection calls for: informational only, never a refusal —
// relaying through a shared hub is completely normal, not suspicious on
// its own, so this is reported, not warned about per-patch.
type importStats struct {
	Fetched        int
	AuthoredByPeer int // signed, and by the fingerprint this connection verified
	Relayed        int // signed, but by a different fingerprint — the peer is relaying someone else's patch
	Unsigned       int
}

// importClosure fetches root and everything it transitively depends on
// (patches, and any blobs a fetched patch's KindBlob changes reference)
// that isn't already present locally, verifying each object's content
// actually hashes to the hash it was requested by before storing it.
// Content addressing is what makes this a plain recursive pull with no
// separate have/want negotiation: "do I have this hash" is the only
// question that ever needs asking, and Store.Has answers it locally.
//
// peerFingerprint is this connection's already-TLS-verified peer
// identity (from dialPeer) — cross-checked against each newly-fetched
// patch's own AuthorFingerprint (already independently verified by
// fetchPatch's VerifyAuthorSignature call) purely for the returned
// importStats; it never changes whether a patch is accepted.
func importClosure(c *client.Client, store *patches.Store, blobs *patches.BlobStore, root patches.Hash, peerFingerprint string) (importStats, error) {
	var stats importStats
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
		stats.Fetched++
		switch {
		case p.AuthorFingerprint == ([32]byte{}):
			stats.Unsigned++
		case auth.Fingerprint(ed25519.PublicKey(p.AuthorFingerprint[:])) == peerFingerprint:
			stats.AuthoredByPeer++
		default:
			stats.Relayed++
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
	err := walk(root)
	return stats, err
}

// printImportStats prints the AuthorFingerprint-vs-peer cross-check
// summary, but only when there was anything to say: a no-op fetch (every
// object was already present locally) prints nothing, matching the
// "informational, not noisy" stance — relaying is the common case, and a
// line on every single up-to-date check would just be clutter.
func printImportStats(stats importStats, peerFingerprint string) {
	if stats.Fetched == 0 {
		return
	}
	fmt.Printf("fetched %d patch(es) from %s: %d authored by this peer, %d relayed from elsewhere, %d unsigned\n",
		stats.Fetched, peerFingerprint[:12], stats.AuthoredByPeer, stats.Relayed, stats.Unsigned)
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
	// A different check from the hash comparison above: that one proves
	// these bytes are what's stored under this hash (transit integrity),
	// not that the claimed authorship is real — a dishonest peer can
	// craft arbitrary content and correctly self-hash it, but can't
	// produce a valid signature for a fingerprint it doesn't hold the
	// private key for. An unsigned patch (no fingerprint claimed) always
	// passes this; only a present-but-invalid signature is refused.
	if !p.VerifyAuthorSignature() {
		return nil, fmt.Errorf("patch %s: claims authorship by fingerprint %x but its signature doesn't verify — possible forgery", hash, p.AuthorFingerprint)
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
