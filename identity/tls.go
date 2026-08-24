package identity

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

// TLSCertificate returns the tls.Certificate for this identity, for
// tls.Config.Certificates — both server and client present one, since the
// server requires a client certificate too (see ServerTLSConfig).
func (id *Identity) TLSCertificate() tls.Certificate {
	return tls.Certificate{Certificate: [][]byte{id.Raw}, PrivateKey: id.Key}
}

// ServerTLSConfig returns a tls.Config for `9vcs serve`: TLS 1.3 only, our
// own certificate, and client-certificate authentication via accept
// instead of the usual CA-chain verification — there is no CA in this
// design at all (see package doc). RequireAnyClientCert means Go performs
// no chain verification of its own (there's no ClientCAs pool to verify
// against); accept is the only gate, and an unauthorized fingerprint fails
// the handshake itself, before Attach is ever reachable.
func (id *Identity) ServerTLSConfig(accept func(fingerprint string) bool) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{id.TLSCertificate()},
		ClientAuth:         tls.RequireAnyClientCert,
		InsecureSkipVerify: true, // no CA chain to build in the first place; accept below is the real gate
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			fp, err := fingerprintOfRaw(rawCerts)
			if err != nil {
				return err
			}
			if !accept(fp) {
				return fmt.Errorf("identity: peer %s is not an authorized peer", fp)
			}
			return nil
		},
	}
}

// ClientTLSConfig returns a tls.Config for connecting to a peer: TLS 1.3
// only, our own certificate, and server-certificate verification via
// accept instead of a CA chain.
func (id *Identity) ClientTLSConfig(accept func(fingerprint string) bool) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{id.TLSCertificate()},
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			fp, err := fingerprintOfRaw(rawCerts)
			if err != nil {
				return err
			}
			if !accept(fp) {
				return fmt.Errorf("identity: server %s is not trusted", fp)
			}
			return nil
		},
	}
}

func fingerprintOfRaw(rawCerts [][]byte) (string, error) {
	if len(rawCerts) == 0 {
		return "", fmt.Errorf("identity: peer presented no certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return "", fmt.Errorf("identity: parsing peer certificate: %w", err)
	}
	return FingerprintOf(cert)
}
