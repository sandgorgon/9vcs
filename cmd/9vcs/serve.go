package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"os"

	"github.com/sandgorgon/9p/server"

	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/vcsfs"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("serve: expected exactly one address to listen on, e.g. :4921")
	}
	addr := rest[0]

	r, err := findRepo()
	if err != nil {
		return err
	}
	id, err := identity.Load()
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	authorized, err := identity.LoadAuthorizedPeers(r.authorizedPeersFile())
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	if len(authorized) == 0 {
		fmt.Fprintf(os.Stderr, "9vcs serve: warning: %s is empty or missing; no peer will be able to connect\n", r.authorizedPeersFile())
	}

	// One shared config, not one per connection: authorized never changes
	// mid-process, and tls.Config is safe to reuse across connections.
	tlsCfg := id.ServerTLSConfig(func(fp string) bool { return authorized.Allows(fp, identity.PermRead) })

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	defer ln.Close()
	fmt.Printf("serving %s on %s, identity %s\n", r.root, ln.Addr(), id.Fingerprint())

	for {
		conn, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("serve: accept: %w", err)
		}
		go handlePeer(r, conn, tlsCfg, authorized)
	}
}

// handlePeer is deliberately NOT server.Server.Serve's own accept loop —
// per PLAN.md, Serve(l) never exposes the underlying connection, so there
// is no hook for a verified peer identity to reach FileSystem.Attach.
// Running the accept loop here instead, TLS-handshaking each connection
// by hand before constructing a vcsfs.FS for it, is how that identity
// actually gets threaded through: ServerTLSConfig's VerifyPeerCertificate
// already refused anything not in authorized during the handshake itself,
// so by the time this function's fingerprint lookup runs, it's guaranteed
// to be present.
func handlePeer(r *repo, conn net.Conn, tlsCfg *tls.Config, authorized identity.AuthorizedPeers) {
	defer conn.Close()
	tlsConn := tls.Server(conn, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		fmt.Fprintf(os.Stderr, "9vcs serve: %s: handshake failed: %v\n", conn.RemoteAddr(), err)
		return
	}
	peerCerts := tlsConn.ConnectionState().PeerCertificates
	if len(peerCerts) == 0 {
		fmt.Fprintf(os.Stderr, "9vcs serve: %s: no peer certificate presented\n", conn.RemoteAddr())
		return
	}
	fp, err := identity.FingerprintOf(peerCerts[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "9vcs serve: %s: %v\n", conn.RemoteAddr(), err)
		return
	}
	perm := authorized[fp]
	fmt.Printf("9vcs serve: %s connected as %s (%s)\n", conn.RemoteAddr(), fp, perm)

	fsys := &vcsfs.FS{Store: r.store, Blobs: r.blobs, Refs: refAdapter{r}, Perm: perm}
	srv := &server.Server{FS: fsys}
	if err := srv.ServeConn(tlsConn); err != nil {
		fmt.Printf("9vcs serve: %s disconnected: %v\n", conn.RemoteAddr(), err)
	}
}
