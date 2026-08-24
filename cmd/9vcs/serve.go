package main

import (
	"context"
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

	tlsCfg := id.ServerTLSConfig(func(fp string) bool { return authorized.Allows(fp, identity.PermRead) })

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	defer ln.Close()
	fmt.Printf("serving %s on %s, identity %s\n", r.root, ln.Addr(), id.Fingerprint())

	// One shared FS for every connection: unlike before ConnContext
	// existed, per-connection identity no longer needs its own FS
	// instance — it flows through the request context instead (see
	// ConnContext below and vcsfs.WithPermission).
	fsys := &vcsfs.FS{Store: r.store, Blobs: r.blobs, Refs: refAdapter{r}}
	srv := &server.Server{
		FS: fsys,
		// ConnContext is what replaces the hand-rolled accept loop this
		// command used before v0.2.0: Serve itself now TLS-handshakes
		// each connection (via tlsListener below) and calls this once per
		// connection, before any 9P messages are read on it, to attach
		// the verified peer's permission to that connection's requests.
		ConnContext: func(ctx context.Context, nc net.Conn) context.Context {
			tlsConn, ok := nc.(*tls.Conn)
			if !ok {
				return ctx
			}
			// tls.Conn handshakes lazily on first Read/Write; force it now
			// so ConnectionState().PeerCertificates is actually populated
			// by the time Attach runs. authorized.Allows already gated
			// this during the handshake (ServerTLSConfig's
			// VerifyPeerCertificate), so failure here just means Attach
			// finds no permission in the context and refuses — the
			// connection was already headed nowhere either way.
			if err := tlsConn.Handshake(); err != nil {
				fmt.Fprintf(os.Stderr, "9vcs serve: %s: handshake failed: %v\n", nc.RemoteAddr(), err)
				return ctx
			}
			certs := tlsConn.ConnectionState().PeerCertificates
			if len(certs) == 0 {
				return ctx
			}
			fp, err := identity.FingerprintOf(certs[0])
			if err != nil {
				return ctx
			}
			perm := authorized[fp]
			fmt.Printf("9vcs serve: %s connected as %s (%s)\n", nc.RemoteAddr(), fp, perm)
			return vcsfs.WithPermission(ctx, perm)
		},
	}
	if err := srv.Serve(tls.NewListener(ln, tlsCfg)); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
