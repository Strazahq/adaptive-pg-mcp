package core

import (
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// certReload is the least time between two reads of the certificate and key files while serve runs.
var certReload = time.Minute

// pairFault says why a certificate and key do not load: what failed, and what the operator does about it.
type pairFault struct {
	what, fix string
}

// refusal is the sentence of a pair that refuses the start.
func (f *pairFault) refusal() string {
	return f.what + ", so adaptive-pg-mcp will not start. " + f.fix
}

// kept is the sentence of a new pair that does not load while serve keeps the pair it has.
func (f *pairFault) kept() string {
	return f.what + ", so adaptive-pg-mcp keeps serving the certificate it loaded before. " + f.fix
}

// checkTLS is the TLS part of the start checks. With tls in server.yaml, a
// certificate and key that do not load, or a certificate that has expired,
// refuse the start, because serve could not serve HTTPS with them. Without
// tls, it reports plain HTTP on an address beyond this machine, which only
// allow_plain_http: true accepts, because the bearer tokens then cross the
// network in cleartext.
func (s *Server) checkTLS() []Finding {
	switch {
	case s.cfg.TLS != nil:
		if _, _, fault := loadPair(*s.cfg.TLS, time.Now()); fault != nil {
			return []Finding{{Refuses: true, Text: fault.refusal()}}
		}
	case s.cfg.AllowPlainHTTP && !config.OnThisMachine(s.cfg.Listen):
		return []Finding{{Text: "listen " + s.cfg.Listen + " serves plain HTTP beyond this machine, which allow_plain_http: true accepts. The bearer tokens of the callers cross the network in cleartext unless something in front of the server encrypts the traffic, such as a service mesh. Set tls with cert_file and key_file to serve HTTPS instead."}}
	}
	return nil
}

// keyPair is the certificate and key that HTTPS serves. At a handshake
// after interval has passed since the last read, it reads both files again,
// and from that handshake on it serves the pair they hold when the pair
// loads. A pair that does not load leaves the one before in service, with
// one warning per change of the files.
type keyPair struct {
	files    config.TLS
	log      *slog.Logger
	interval time.Duration

	mu sync.Mutex
	// cert is the pair that each handshake serves.
	cert *tls.Certificate
	// seen identifies what the files held at the last read, so that a read of the same files changes nothing and logs nothing.
	seen string
	// next is the earliest time of the next read.
	next time.Time
}

// newKeyPair loads the pair that files names, which it reads again at most once per certReload, or returns why it does not load.
func newKeyPair(files config.TLS, log *slog.Logger, now time.Time) (*keyPair, *pairFault) {
	cert, seen, fault := loadPair(files, now)
	if fault != nil {
		return nil, fault
	}
	return &keyPair{files: files, log: log, interval: certReload, cert: cert, seen: seen, next: now.Add(certReload)}, nil
}

// tlsConfig is the TLS of serve: TLS 1.2 or later, Go's default cipher
// suites and curves, no client certificates, and the pair of p at each
// handshake. http.Server.ServeTLS adds HTTP/2 and HTTP/1.1 by ALPN.
func (p *keyPair) tlsConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: p.certificate}
}

// certificate returns the pair for a handshake, after it reads the files again when interval has passed.
func (p *keyPair) certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now := time.Now(); !now.Before(p.next) {
		p.next = now.Add(p.interval)
		p.reload(now)
	}
	return p.cert, nil
}

// reload reads the files again. When they hold something else than at the
// last read, it serves their pair if it loads, and logs once either way.
func (p *keyPair) reload(now time.Time) {
	cert, seen, fault := loadPair(p.files, now)
	if seen == p.seen {
		return
	}
	p.seen = seen
	if fault != nil {
		p.log.Warn(fault.kept())
		return
	}
	p.cert = cert
	p.log.Info("serving a new certificate and key from the next TLS handshake", "cert_file", p.files.CertFile, "key_file", p.files.KeyFile)
}

// loadPair reads the certificate and key files, and checks that they hold a
// certificate and its own private key in PEM form, and that the leaf
// certificate has not expired at now. It also returns what identifies the
// files as read: a digest of both, or the read failure. No sentence repeats
// the contents of either file.
func loadPair(files config.TLS, now time.Time) (*tls.Certificate, string, *pairFault) {
	certPEM, fault := readFile(files.CertFile, "certificate file", "tls.cert_file")
	if fault != nil {
		return nil, fault.what, fault
	}
	keyPEM, fault := readFile(files.KeyFile, "key file", "tls.key_file")
	if fault != nil {
		return nil, fault.what, fault
	}
	seen := digest(certPEM) + digest(keyPEM)
	certs := blocks(certPEM, func(t string) bool { return t == "CERTIFICATE" })
	if len(certs) == 0 {
		return nil, seen, &pairFault{"the certificate file " + files.CertFile + " holds no certificate in PEM form", "Put the server's certificate in it in PEM form, followed by any intermediate certificates."}
	}
	keys := blocks(keyPEM, func(t string) bool { return t == "PRIVATE KEY" || strings.HasSuffix(t, " PRIVATE KEY") })
	if len(keys) == 0 {
		return nil, seen, &pairFault{"the key file " + files.KeyFile + " holds no private key in PEM form", "Put the private key of the certificate in it, in PEM form."}
	}
	leaf, err := x509.ParseCertificate(certs[0])
	if err != nil {
		return nil, seen, &pairFault{"the first certificate in " + files.CertFile + " does not parse as an X.509 certificate", "Replace it with the server's certificate as its issuer gave it."}
	}
	key, ok := parseKey(keys[0])
	if !ok {
		return nil, seen, &pairFault{"the private key in " + files.KeyFile + " does not parse as an RSA, ECDSA or Ed25519 key", "Replace it with the private key of the certificate in " + files.CertFile + ", in PEM form."}
	}
	if pub, ok := key.Public().(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(leaf.PublicKey) {
		return nil, seen, &pairFault{"the private key in " + files.KeyFile + " does not belong to the certificate in " + files.CertFile, "Put a certificate and its own private key in the two files."}
	}
	if now.After(leaf.NotAfter) {
		return nil, seen, &pairFault{"the certificate in " + files.CertFile + " expired at " + leaf.NotAfter.UTC().Format(time.RFC3339), "Renew it, and put the new certificate and its private key in " + files.CertFile + " and " + files.KeyFile + "."}
	}
	return &tls.Certificate{Certificate: certs, PrivateKey: key, Leaf: leaf}, seen, nil
}

// readFile reads one file of the pair. Its sentence names the file and the
// key of server.yaml that names it, and the reason from the system.
func readFile(path, file, key string) ([]byte, *pairFault) {
	data, err := os.ReadFile(path)
	if err == nil {
		return data, nil
	}
	reason := err
	if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
		reason = pe.Err
	}
	return nil, &pairFault{"the " + file + " " + path + ", which " + key + " names, cannot be read: " + reason.Error(), "Check that the file exists and that the user adaptive-pg-mcp runs as may read it."}
}

// digest is the SHA-256 of data in hex.
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// blocks returns the bytes of every PEM block in data whose type keep accepts, in file order.
func blocks(data []byte, keep func(string) bool) [][]byte {
	var out [][]byte
	for {
		var b *pem.Block
		if b, data = pem.Decode(data); b == nil {
			return out
		}
		if keep(b.Type) {
			out = append(out, b.Bytes)
		}
	}
}

// parseKey reads a private key in PKCS #8, PKCS #1 or SEC 1 form, as
// crypto/tls reads them, and reports whether it is a key that signs.
func parseKey(der []byte) (crypto.Signer, bool) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		s, ok := k.(crypto.Signer)
		return s, ok
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, true
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, true
	}
	return nil, false
}
