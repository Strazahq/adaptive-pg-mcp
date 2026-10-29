package core

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/strazahq/adaptive-pg-mcp/internal/config"
)

// lockedLog is a log that a running server writes while the test reads it.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends p to the log.
func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// String returns the log so far.
func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// pairFiles writes a new self-signed certificate for 127.0.0.1 with the
// common name name, valid until notAfter, and its private key into dir as
// name.crt and name.key. The key exists only in the test's own folder.
func pairFiles(t *testing.T, dir, name string, notAfter time.Time) (config.TLS, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    notAfter.Add(-30 * 24 * time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	files := config.TLS{CertFile: filepath.Join(dir, name+".crt"), KeyFile: filepath.Join(dir, name+".key")}
	writeFile(t, files.CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, files.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	return files, leaf
}

// writeFile writes data to path in place, as a renewal tool replaces a file.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// copyFile writes the contents of from over to.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, to, data)
}

func TestCheckTLS(t *testing.T) {
	dir := t.TempDir()
	day := time.Now().Add(24 * time.Hour)
	good, _ := pairFiles(t, dir, "good", day)
	other, _ := pairFiles(t, dir, "other", day)
	expired, old := pairFiles(t, dir, "expired", time.Now().Add(-time.Hour))
	notPEM := filepath.Join(dir, "notes.txt")
	writeFile(t, notPEM, []byte("this file holds no PEM block\n"))
	badCert := filepath.Join(dir, "bad.crt")
	writeFile(t, badCert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not DER")}))
	badKey := filepath.Join(dir, "bad.key")
	writeFile(t, badKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("not DER")}))
	missing := filepath.Join(dir, "missing.crt")
	const stop = ". "
	readable := "Check that it exists and that the user the server runs as can read it."
	cases := []struct {
		name  string
		files config.TLS
		want  string
	}{
		{"a valid pair", good, ""},
		{"an expired certificate", expired, "the certificate in " + expired.CertFile + " expired at " + old.NotAfter.UTC().Format(time.RFC3339) + stop +
			"Renew it, and put the new certificate and key in " + expired.CertFile + " and " + expired.KeyFile + "."},
		{"the key of another certificate", config.TLS{CertFile: good.CertFile, KeyFile: other.KeyFile},
			"the private key in " + other.KeyFile + " does not match the certificate in " + good.CertFile + stop + "Put a certificate and its own private key in the two files."},
		{"a certificate file without PEM", config.TLS{CertFile: notPEM, KeyFile: good.KeyFile},
			"the certificate file " + notPEM + " holds no PEM certificate" + stop + "Put the server certificate in it as PEM, followed by any intermediate certificates."},
		{"the two files switched", config.TLS{CertFile: good.KeyFile, KeyFile: good.CertFile},
			"the certificate file " + good.KeyFile + " holds no PEM certificate" + stop + "Put the server certificate in it as PEM, followed by any intermediate certificates."},
		{"a key file without PEM", config.TLS{CertFile: good.CertFile, KeyFile: notPEM},
			"the key file " + notPEM + " holds no PEM private key" + stop + "Put the certificate's private key in it as PEM."},
		{"a certificate that does not parse", config.TLS{CertFile: badCert, KeyFile: good.KeyFile},
			"the first certificate in " + badCert + " does not parse as an X.509 certificate" + stop + "Replace it with the server certificate as issued."},
		{"a key that does not parse", config.TLS{CertFile: good.CertFile, KeyFile: badKey},
			"the private key in " + badKey + " does not parse as an RSA, ECDSA or Ed25519 key" + stop + "Replace it with the PEM private key of the certificate in " + good.CertFile + "."},
		{"a missing certificate file", config.TLS{CertFile: missing, KeyFile: good.KeyFile},
			"the certificate file " + missing + " (tls.cert_file) cannot be read: no such file or directory" + stop + readable},
		{"a key file that is a folder", config.TLS{CertFile: good.CertFile, KeyFile: dir},
			"the key file " + dir + " (tls.key_file) cannot be read: is a directory" + stop + readable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := New(&config.Config{Mode: config.Straza, Listen: "127.0.0.1:0", TLS: &c.files}, slog.New(slog.DiscardHandler))
			findings := s.Check(context.Background())
			s.Close()
			switch {
			case c.want == "" && len(findings) != 0:
				t.Errorf("the start checks found %+v, want nothing", findings)
			case c.want != "" && (len(findings) != 1 || !findings[0].Refuses || findings[0].Text != c.want):
				t.Errorf("the start checks found %+v, want the refusal:\n%s", findings, c.want)
			}
		})
	}
}

func TestCheckReportsPlainHTTPBeyondThisMachine(t *testing.T) {
	cases := []struct {
		name, listen, want string
	}{
		{"every interface", "0.0.0.0:8080", "listen 0.0.0.0:8080 serves plain HTTP beyond this machine, which allow_plain_http: true accepts. Bearer tokens cross the network in cleartext unless a proxy or service mesh encrypts the traffic. Set tls.cert_file and tls.key_file to serve HTTPS."},
		{"loopback", "127.0.0.1:8080", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := New(&config.Config{Mode: config.Straza, Listen: c.listen, AllowPlainHTTP: true}, slog.New(slog.DiscardHandler))
			findings := s.Check(context.Background())
			s.Close()
			switch {
			case c.want == "" && len(findings) != 0:
				t.Errorf("the start checks found %+v, want nothing", findings)
			case c.want != "" && (len(findings) != 1 || findings[0].Refuses || findings[0].Text != c.want):
				t.Errorf("the start checks found %+v, want the report:\n%s", findings, c.want)
			}
		})
	}
}

// serveTLS runs Serve with the whoami tool and the pair files on
// 127.0.0.1:0 until the test ends, and returns the endpoint URL it logs and
// its log.
func serveTLS(t *testing.T, files config.TLS) (string, *lockedLog) {
	t.Helper()
	cfg := standaloneConfig()
	cfg.TLS = &files
	return serveConfig(t, cfg)
}

// serveConfig runs Serve for cfg with the whoami tool on 127.0.0.1:0, whatever cfg.Listen says, until the test ends.
func serveConfig(t *testing.T, cfg *config.Config) (string, *lockedLog) {
	t.Helper()
	logs := &lockedLog{}
	s := New(cfg, slog.New(slog.NewTextHandler(logs, nil)))
	whoami(s)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	address := regexp.MustCompile(`address=(\S+)`)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if m := address.FindStringSubmatch(logs.String()); m != nil {
			return m[1], logs
		}
	}
	t.Fatalf("Serve logged no address:\n%s", logs.String())
	return "", nil
}

func TestServeWarnsOfPlainHTTPBeyondThisMachine(t *testing.T) {
	cfg := standaloneConfig()
	cfg.Listen, cfg.AllowPlainHTTP = "0.0.0.0:8080", true
	url, logs := serveConfig(t, cfg)
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("Serve logged the address %s, want http://127.0.0.1 and a port", url)
	}
	const warn = `level=WARN msg="listen 0.0.0.0:8080 serves plain HTTP beyond this machine, which allow_plain_http: true accepts. Bearer tokens cross the network in cleartext unless a proxy or service mesh encrypts the traffic. Set tls.cert_file and tls.key_file to serve HTTPS."`
	if !strings.Contains(logs.String(), warn) {
		t.Errorf("the start log does not warn of plain HTTP:\n%s", logs.String())
	}
}

// trusting is a certificate pool that holds the leaves.
func trusting(leaves ...*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, l := range leaves {
		pool.AddCert(l)
	}
	return pool
}

// tlsBearer adds a bearer token to every request of the SDK client and sends it on base.
type tlsBearer struct {
	token string
	base  http.RoundTripper
}

// RoundTrip sends r with the token in its Authorization header.
func (b tlsBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func TestServeOverTLS(t *testing.T) {
	files, leaf := pairFiles(t, t.TempDir(), "server", time.Now().Add(24*time.Hour))
	url, _ := serveTLS(t, files)
	addr, ok := strings.CutSuffix(strings.TrimPrefix(url, "https://"), endpointPath)
	if !ok || !strings.HasPrefix(url, "https://127.0.0.1:") {
		t.Fatalf("Serve logged the address %s, want https://127.0.0.1 and a port", url)
	}
	roots := trusting(leaf)
	base := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}, ForceAttemptHTTP2: true}
	t.Cleanup(base.CloseIdleConnections)

	t.Run("a call with a valid token works", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: tlsBearer{tokenA, base}}, MaxRetries: -1}
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, tr, nil)
		if err != nil {
			t.Fatalf("connect over HTTPS: %v", err)
		}
		defer cs.Close()
		if got := callWhoami(t, cs); got != "support_bot" {
			t.Errorf("whoami over HTTPS answered %q, want support_bot", got)
		}
	})

	t.Run("a call without a token gets 401 over HTTP/2", func(t *testing.T) {
		r := opening(mcp.SupportedProtocolVersions()[0])
		req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(r.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		res, err := (&http.Client{Transport: base, Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("POST over HTTPS: %v", err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized || res.Proto != "HTTP/2.0" {
			t.Errorf("a request without a token answered %d over %s, want 401 over HTTP/2.0", res.StatusCode, res.Proto)
		}
	})

	t.Run("plain HTTP gets no MCP answer", func(t *testing.T) {
		r := opening(mcp.SupportedProtocolVersions()[0], "Bearer "+tokenA)
		a := send(t, "http://"+addr+endpointPath, r)
		if a.status != http.StatusBadRequest || strings.Contains(a.body, "jsonrpc") {
			t.Errorf("plain HTTP to the HTTPS port answered %d %q, want 400 and no MCP answer", a.status, a.body)
		}
	})

	handshakes := []struct {
		name     string
		min, max uint16
		protos   []string
		want     string
	}{
		{"TLS 1.1 is refused", tls.VersionTLS10, tls.VersionTLS11, []string{"h2", "http/1.1"}, ""},
		{"TLS 1.2 negotiates h2", tls.VersionTLS12, tls.VersionTLS12, []string{"h2", "http/1.1"}, "h2"},
		{"TLS 1.3 negotiates h2", tls.VersionTLS13, tls.VersionTLS13, []string{"h2", "http/1.1"}, "h2"},
		{"an HTTP/1.1 client negotiates http/1.1", 0, 0, []string{"http/1.1"}, "http/1.1"},
	}
	for _, h := range handshakes {
		t.Run(h.name, func(t *testing.T) {
			asked := false
			cfg := &tls.Config{RootCAs: roots, MinVersion: h.min, MaxVersion: h.max, NextProtos: h.protos,
				GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
					asked = true
					return &tls.Certificate{}, nil
				}}
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, cfg)
			if h.want == "" {
				if err == nil || !strings.Contains(err.Error(), "protocol version not supported") {
					t.Errorf("the handshake answered %v, want the server to refuse the protocol version", err)
				}
				if err == nil {
					conn.Close()
				}
				return
			}
			if err != nil {
				t.Fatalf("the handshake failed: %v", err)
			}
			defer conn.Close()
			if got := conn.ConnectionState().NegotiatedProtocol; got != h.want {
				t.Errorf("ALPN negotiated %q, want %q", got, h.want)
			}
			if asked {
				t.Errorf("the server asked for a client certificate")
			}
		})
	}
}

// servedName makes one TLS handshake with addr and returns the common name of the certificate it serves.
func servedName(t *testing.T, addr string, roots *x509.CertPool) string {
	t.Helper()
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, &tls.Config{RootCAs: roots})
	if err != nil {
		t.Fatalf("the handshake failed: %v", err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0].Subject.CommonName
}

// reloadEvery sets certReload to d until the test ends. It is called before
// serveTLS, so that the server stops before certReload is restored.
func reloadEvery(t *testing.T, d time.Duration) {
	before := certReload
	certReload = d
	t.Cleanup(func() { certReload = before })
}

func TestServeRenewsThePair(t *testing.T) {
	reloadEvery(t, 0)
	dir := t.TempDir()
	day := time.Now().Add(24 * time.Hour)
	files, first := pairFiles(t, dir, "first", day)
	second, secondLeaf := pairFiles(t, dir, "second", day)
	third, _ := pairFiles(t, dir, "third", day)
	fourth, fourthLeaf := pairFiles(t, dir, "fourth", day)
	url, logs := serveTLS(t, files)
	addr := strings.TrimSuffix(strings.TrimPrefix(url, "https://"), endpointPath)
	roots := trusting(first, secondLeaf, fourthLeaf)
	mismatch := "the private key in " + files.KeyFile + " does not match the certificate in " + files.CertFile + ", so the server keeps serving the previous certificate. Put a certificate and its own private key in the two files."
	notPEM := "the certificate file " + files.CertFile + " holds no PEM certificate, so the server keeps serving the previous certificate. Put the server certificate in it as PEM, followed by any intermediate certificates."
	steps := []struct {
		name   string
		change func()
		served string
		warned []string
		infos  int
	}{
		{"the first pair", func() {}, "first", nil, 0},
		{"a renewed pair", func() { copyFile(t, second.CertFile, files.CertFile); copyFile(t, second.KeyFile, files.KeyFile) }, "second", nil, 1},
		{"the same files again", func() {}, "second", nil, 1},
		{"the key of another certificate", func() { copyFile(t, third.KeyFile, files.KeyFile) }, "second", []string{mismatch}, 1},
		{"the broken pair again", func() {}, "second", []string{mismatch}, 1},
		{"a certificate file without PEM", func() { writeFile(t, files.CertFile, []byte("renewal in progress\n")) }, "second", []string{mismatch, notPEM}, 1},
		{"a pair that loads again", func() { copyFile(t, fourth.CertFile, files.CertFile); copyFile(t, fourth.KeyFile, files.KeyFile) }, "fourth", []string{mismatch, notPEM}, 2},
	}
	warning := regexp.MustCompile(`level=WARN msg="(.*keeps serving.*)"$`)
	for _, step := range steps {
		step.change()
		if got := servedName(t, addr, roots); got != step.served {
			t.Errorf("%s: the handshake served %s, want %s", step.name, got, step.served)
		}
		var warned []string
		for _, line := range strings.Split(logs.String(), "\n") {
			if m := warning.FindStringSubmatch(line); m != nil {
				warned = append(warned, m[1])
			}
		}
		if strings.Join(warned, "\n") != strings.Join(step.warned, "\n") {
			t.Errorf("%s: the log warns:\n%s\nwant:\n%s", step.name, strings.Join(warned, "\n"), strings.Join(step.warned, "\n"))
		}
		if got := strings.Count(logs.String(), `level=INFO msg="serving a new certificate and key from the next TLS handshake" cert_file=`+files.CertFile+" key_file="+files.KeyFile); got != step.infos {
			t.Errorf("%s: the log says %d times that it serves a new pair, want %d:\n%s", step.name, got, step.infos, logs.String())
		}
	}
}

func TestServeReadsThePairAtMostOncePerInterval(t *testing.T) {
	reloadEvery(t, time.Hour)
	dir := t.TempDir()
	day := time.Now().Add(24 * time.Hour)
	files, first := pairFiles(t, dir, "first", day)
	second, secondLeaf := pairFiles(t, dir, "second", day)
	url, logs := serveTLS(t, files)
	addr := strings.TrimSuffix(strings.TrimPrefix(url, "https://"), endpointPath)
	copyFile(t, second.CertFile, files.CertFile)
	copyFile(t, second.KeyFile, files.KeyFile)
	for range 3 {
		if got := servedName(t, addr, trusting(first, secondLeaf)); got != "first" {
			t.Fatalf("a handshake within the interval served %s, want first", got)
		}
	}
	if strings.Contains(logs.String(), "serving a new certificate") {
		t.Errorf("the log says it serves a new pair within the interval:\n%s", logs.String())
	}
}

func TestServeRefusesAPairThatDoesNotLoad(t *testing.T) {
	dir := t.TempDir()
	good, _ := pairFiles(t, dir, "good", time.Now().Add(24*time.Hour))
	other, _ := pairFiles(t, dir, "other", time.Now().Add(24*time.Hour))
	expired, old := pairFiles(t, dir, "expired", time.Now().Add(-time.Hour))
	cases := []struct {
		name  string
		files config.TLS
		want  string
	}{
		{"an expired certificate", expired, "the certificate in " + expired.CertFile + " expired at " + old.NotAfter.UTC().Format(time.RFC3339) +
			". Renew it, and put the new certificate and key in " + expired.CertFile + " and " + expired.KeyFile + "."},
		{"the key of another certificate", config.TLS{CertFile: good.CertFile, KeyFile: other.KeyFile},
			"the private key in " + other.KeyFile + " does not match the certificate in " + good.CertFile + ". Put a certificate and its own private key in the two files."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := &lockedLog{}
			cfg := standaloneConfig()
			cfg.TLS = &c.files
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			err = New(cfg, slog.New(slog.NewTextHandler(logs, nil))).Serve(context.Background(), ln)
			const refused = "adaptive-pg-mcp will not start, because 1 start check refused it. Fix what the log lines above say, and run adaptive-pg-mcp check to test the fix without serving."
			if err == nil || err.Error() != refused {
				t.Errorf("Serve answered %v, want: %s", err, refused)
			}
			if !strings.Contains(logs.String(), `level=ERROR msg="`+c.want+`"`) {
				t.Errorf("the log does not hold the refusal %q:\n%s", c.want, logs.String())
			}
			if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Errorf("the listener still accepts after the refusal: %v", err)
			}
			if strings.Contains(logs.String(), "serving Streamable HTTP") {
				t.Errorf("Serve served after the refusal:\n%s", logs.String())
			}
		})
	}
}

// boundTo is a listener whose Addr reports addr, as a listener of a name that resolves there would.
type boundTo struct {
	net.Listener
	addr net.Addr
}

// Addr returns the address the test gives it.
func (b boundTo) Addr() net.Addr { return b.addr }

func TestServeRefusesPlainHTTPOffLoopback(t *testing.T) {
	files, _ := pairFiles(t, t.TempDir(), "server", time.Now().Add(24*time.Hour))
	elsewhere := &net.TCPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 8080}
	cases := []struct {
		name  string
		addr  net.Addr
		tls   bool
		plain bool
		want  string
	}{
		{"plain HTTP on an address of another network", elsewhere, false, false,
			"adaptive-pg-mcp will not start, because listen localhost:8080 resolved to 192.0.2.10:8080, not a loopback address, and without tls the tokens would cross the network in cleartext. Make localhost resolve to 127.0.0.1, set listen to 127.0.0.1:8080, or set tls.cert_file and tls.key_file."},
		{"plain HTTP on an IPv6 link-local address", &net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 8080, Zone: "eth0"}, false, false,
			"adaptive-pg-mcp will not start, because listen localhost:8080 resolved to [fe80::1%eth0]:8080, not a loopback address, and without tls the tokens would cross the network in cleartext. Make localhost resolve to 127.0.0.1, set listen to 127.0.0.1:8080, or set tls.cert_file and tls.key_file."},
		{"plain HTTP on ::1", &net.TCPAddr{IP: net.IPv6loopback, Port: 8080}, false, false, ""},
		{"plain HTTP that allow_plain_http accepts", elsewhere, false, true, ""},
		{"HTTPS", elsewhere, true, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := &lockedLog{}
			cfg := standaloneConfig()
			cfg.Listen, cfg.AllowPlainHTTP = "localhost:8080", c.plain
			if c.tls {
				cfg.TLS = &files
			}
			real, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			served := make(chan error, 1)
			go func() { served <- New(cfg, slog.New(slog.NewTextHandler(logs, nil))).Serve(ctx, boundTo{real, c.addr}) }()
			if c.want != "" {
				if err := <-served; err == nil || err.Error() != c.want {
					t.Errorf("Serve answered %v, want: %s", err, c.want)
				}
				if _, err := real.Accept(); !errors.Is(err, net.ErrClosed) {
					t.Errorf("the listener still accepts after the refusal: %v", err)
				}
				if strings.Contains(logs.String(), "the start checks passed") {
					t.Errorf("the start checks ran before the refusal:\n%s", logs.String())
				}
				return
			}
			for deadline := time.Now().Add(10 * time.Second); !strings.Contains(logs.String(), "serving Streamable HTTP"); time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("Serve did not serve:\n%s", logs.String())
				}
			}
			cancel()
			if err := <-served; err != nil {
				t.Errorf("Serve: %v", err)
			}
		})
	}
}
