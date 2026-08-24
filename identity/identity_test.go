package identity

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// handshakePair runs a real TLS handshake over an actual localhost TCP
// connection (not net.Pipe: it's fully synchronous/unbuffered, and a
// handshake one side aborts mid-flight deadlocks both ends blocked on
// writes the other has stopped reading — a testing artifact, not
// something a kernel-buffered real connection hits). A deadline bounds
// both sides so a genuine bug fails the test instead of hanging it.
//
// afterHandshake, if not nil, runs on the client's *tls.Conn after
// Handshake returns, regardless of its error — for probing whether the
// connection is actually usable (see TestTLSHandshakeRejectsUnknownPeer).
func handshakePair(t *testing.T, serverCfg, clientCfg *tls.Config, afterHandshake func(*tls.Conn)) (serverErr, clientErr error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			serverErr = err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		serverErr = tls.Server(conn, serverCfg).Handshake()
	}()
	go func() {
		defer wg.Done()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			clientErr = err
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		tlsConn := tls.Client(conn, clientCfg)
		clientErr = tlsConn.Handshake()
		if afterHandshake != nil {
			afterHandshake(tlsConn)
		}
	}()
	wg.Wait()
	return serverErr, clientErr
}

func testIdentity(t *testing.T) *Identity {
	t.Helper()
	dir := t.TempDir()
	id, err := loadFrom(filepath.Join(dir, "identity.key"), filepath.Join(dir, "identity.cert"))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestLoadPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "identity.key")
	certPath := filepath.Join(dir, "identity.cert")

	first, err := loadFrom(keyPath, certPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadFrom(keyPath, certPath)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatalf("reload produced a different fingerprint: %s vs %s", first.Fingerprint(), second.Fingerprint())
	}
}

func TestFingerprintOfMatchesOwnFingerprint(t *testing.T) {
	id := testIdentity(t)
	cert, err := x509.ParseCertificate(id.Raw)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := FingerprintOf(cert)
	if err != nil {
		t.Fatal(err)
	}
	if fp != id.Fingerprint() {
		t.Fatalf("FingerprintOf(own cert) = %s, want %s", fp, id.Fingerprint())
	}
}

// TestTLSHandshakeAccepted: a full TLS 1.3 handshake over a real
// connection, both sides authenticating the other purely by fingerprint
// (no CA), succeeds when each side's accept predicate matches.
func TestTLSHandshakeAccepted(t *testing.T) {
	server := testIdentity(t)
	client := testIdentity(t)

	serverErr, clientErr := handshakePair(t,
		server.ServerTLSConfig(func(fp string) bool { return fp == client.Fingerprint() }),
		client.ClientTLSConfig(func(fp string) bool { return fp == server.Fingerprint() }),
		nil,
	)
	if serverErr != nil {
		t.Errorf("server handshake: %v", serverErr)
	}
	if clientErr != nil {
		t.Errorf("client handshake: %v", clientErr)
	}
}

// TestTLSHandshakeRejectsUnknownPeer: the handshake must fail — on both
// sides — when the accept predicate doesn't recognize the presented
// fingerprint. This is the actual authorization gate in this design;
// getting it wrong means an unauthorized peer reaches Attach.
func TestTLSHandshakeRejectsUnknownPeer(t *testing.T) {
	server := testIdentity(t)
	client := testIdentity(t)
	stranger := testIdentity(t) // server only trusts this one, not client

	var clientReadErr error
	serverErr, clientErr := handshakePair(t,
		server.ServerTLSConfig(func(fp string) bool { return fp == stranger.Fingerprint() }),
		client.ClientTLSConfig(func(fp string) bool { return fp == server.Fingerprint() }),
		func(c *tls.Conn) {
			var buf [1]byte
			_, clientReadErr = c.Read(buf[:])
		},
	)
	if serverErr == nil {
		t.Error("server accepted a client fingerprint it doesn't trust")
	}
	// The client's own Handshake() call is not guaranteed to observe a
	// server-side rejection of its certificate — TLS 1.3 lets the client
	// consider its own handshake done once it's sent Finished, before the
	// server even evaluates the client's cert. The real signal is that
	// the connection is unusable afterward: the server closes it, so the
	// client's first read fails even when clientErr came back nil. Real
	// callers (import) need to handle both shapes of failure.
	if clientErr == nil && clientReadErr == nil {
		t.Error("client's handshake and first read both succeeded despite the server rejecting it")
	}
}
