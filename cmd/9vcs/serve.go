package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/server"

	"github.com/sandgorgon/9vcs/identity"
	"github.com/sandgorgon/9vcs/vcsfs"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	maxConns := fs.Int("max-conns", 64, "reject new connections once this many are live at once")
	maxConnsPerIPPerMin := fs.Int("max-conns-per-ip-per-min", 30, "reject a remote address's new connections once it exceeds this many within a minute")
	maxRequestsPerConn := fs.Int("max-requests-per-conn", 16, "cap how many requests from one connection are dispatched at once (0 = unlimited); a client can still pipeline more, they just wait for a slot")
	maxConnWriteBuffer := fs.Int64("max-conn-write-buffer", 2*vcsfs.MaxObjectSize, "cap how many bytes one connection may have buffered across all its open (un-Tclunk'd) writes at once")
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
	// Both wrappers act on the raw TCP listener, ahead of the TLS
	// handshake tls.NewListener adds below: a rejected connection here
	// never costs a handshake's asymmetric-crypto work, which is exactly
	// what decision #7's hardening item is guarding — see hardening.go.
	ln = newRateLimitListener(ln, newIPRateLimiter(*maxConnsPerIPPerMin, time.Minute))
	ln = newConnCapListener(ln, *maxConns)
	fmt.Printf("serving %s on %s, identity %s\n", r.root, ln.Addr(), id.Fingerprint())

	// One shared FS for every connection: unlike before ConnContext
	// existed, per-connection identity no longer needs its own FS
	// instance — it flows through the request context instead (see
	// ConnContext below and vcsfs.WithPermission).
	fsys := &vcsfs.FS{Store: r.store, Blobs: r.blobs, Refs: refAdapter{r}, Offers: r.offers}
	srv := &server.Server{
		FS: fsys,
		// Pinned explicitly rather than left at the zero value: Msize
		// already defaults to p9.DefaultMsize when unset (decision #7's
		// "Msize capped" is satisfied either way), but leaving it
		// implicit means this server's behavior silently follows
		// whatever the library's default happens to be release to
		// release, instead of a value this command actually chose.
		Msize: p9.DefaultMsize,
		// MaxConcurrentRequests (9p v0.5.0+) is decision #7's "per-
		// connection concurrent-request cap" — a single connection can
		// otherwise pipeline unboundedly many requests without waiting
		// for replies (9P2000's own tag-multiplexing) and drive the
		// server into spawning a concurrent handler for each. Tflush
		// stays exempt from this at the library level, so a client can
		// still cancel a request holding a slot even at the cap.
		MaxConcurrentRequests: uint32(*maxRequestsPerConn),
		// ConnContext is what replaces the hand-rolled accept loop this
		// command used before v0.2.0: Serve itself now TLS-handshakes
		// each connection (via tlsListener below) and calls this once per
		// connection, before any 9P messages are read on it, to attach
		// the verified peer's permission to that connection's requests.
		ConnContext: func(ctx context.Context, nc net.Conn) context.Context {
			// Attached unconditionally, before the TLS/permission checks
			// below (which can return early on failure): every connection
			// that reaches Attach at all, at whatever permission tier,
			// shares one write-buffer budget — see vcsfs.WithWriteBudget.
			ctx = vcsfs.WithWriteBudget(ctx, *maxConnWriteBuffer)
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
