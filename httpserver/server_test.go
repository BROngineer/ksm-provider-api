package httpserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const waitTimeout = 5 * time.Second

type certFiles struct {
	certFile string
	keyFile  string
	pool     *x509.CertPool
}

func writeCert(t *testing.T, dir string, serial int64, modTime time.Time) certFiles {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	f := certFiles{certFile: filepath.Join(dir, "tls.crt"), keyFile: filepath.Join(dir, "tls.key"), pool: x509.NewCertPool()}
	writeFile(t, f.certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), modTime)
	writeFile(t, f.keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), modTime)
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	f.pool.AddCert(cert)
	return f
}

func writeFile(t *testing.T, path string, data []byte, modTime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

type running struct {
	cancel context.CancelFunc
	errc   chan error
	addr   string
}

func startServer(t *testing.T, s *Server) *running {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.Listener = l
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, errc: make(chan error, 1), addr: l.Addr().String()}
	go func() { r.errc <- s.Start(ctx) }()
	t.Cleanup(cancel)
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.errc:
		if err != nil {
			t.Errorf("Start returned %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Start did not return after cancel")
	}
}

func client(pool *x509.CertPool, maxVersion uint16) *http.Client {
	return &http.Client{
		Timeout: waitTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{RootCAs: pool, MaxVersion: maxVersion, MinVersion: tls.VersionTLS10},
		},
	}
}

func get(t *testing.T, c *http.Client, addr string) (*http.Response, error) {
	t.Helper()
	var resp *http.Response
	var err error
	deadline := time.Now().Add(waitTimeout)
	for {
		resp, err = c.Get("https://" + addr + "/")
		if err == nil || time.Now().After(deadline) {
			return resp, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func servedSerial(t *testing.T, addr string, pool *x509.CertPool) int64 {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
}

func TestServesHandlerOverTLS(t *testing.T) {
	certs := writeCert(t, t.TempDir(), 1, time.Now())
	run := startServer(t, &Server{Handler: okHandler(), CertFile: certs.certFile, KeyFile: certs.keyFile})

	resp, err := get(t, client(certs.pool, 0), run.addr)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	run.stop(t)

	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("response = %d %q, want 200 \"ok\"", resp.StatusCode, body)
	}
}

func TestRejectsTLSBelow12(t *testing.T) {
	certs := writeCert(t, t.TempDir(), 1, time.Now())
	run := startServer(t, &Server{Handler: okHandler(), CertFile: certs.certFile, KeyFile: certs.keyFile})
	if _, err := get(t, client(certs.pool, 0), run.addr); err != nil {
		t.Fatalf("server not ready: %v", err)
	}

	if _, err := client(certs.pool, tls.VersionTLS11).Get("https://" + run.addr + "/"); err == nil {
		t.Error("TLS 1.1 handshake succeeded, want it rejected")
	}
	run.stop(t)
}

func TestReloadsRotatedCertificate(t *testing.T) {
	dir := t.TempDir()
	first := writeCert(t, dir, 1, time.Now().Add(-time.Minute))
	run := startServer(t, &Server{Handler: okHandler(), CertFile: first.certFile, KeyFile: first.keyFile})
	if _, err := get(t, client(first.pool, 0), run.addr); err != nil {
		t.Fatalf("server not ready: %v", err)
	}
	if got := servedSerial(t, run.addr, first.pool); got != 1 {
		t.Fatalf("serial = %d, want 1", got)
	}

	second := writeCert(t, dir, 2, time.Now())
	if got := servedSerial(t, run.addr, second.pool); got != 2 {
		t.Errorf("serial after rotation = %d, want 2", got)
	}
	run.stop(t)
}

func TestKeepsServingOldCertificateWhenRotationIsBroken(t *testing.T) {
	dir := t.TempDir()
	first := writeCert(t, dir, 1, time.Now().Add(-time.Minute))
	run := startServer(t, &Server{Handler: okHandler(), CertFile: first.certFile, KeyFile: first.keyFile})
	if _, err := get(t, client(first.pool, 0), run.addr); err != nil {
		t.Fatalf("server not ready: %v", err)
	}

	writeFile(t, first.certFile, []byte("half-written"), time.Now())
	if got := servedSerial(t, run.addr, first.pool); got != 1 {
		t.Errorf("serial = %d, want the previous certificate (1)", got)
	}
	run.stop(t)
}

func TestStartFailsOnInvalidCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "tls.crt")
	keyFile := filepath.Join(dir, "tls.key")
	writeFile(t, certFile, []byte("not a cert"), time.Now())
	writeFile(t, keyFile, []byte("not a key"), time.Now())

	run := startServer(t, &Server{Handler: okHandler(), CertFile: certFile, KeyFile: keyFile})
	select {
	case err := <-run.errc:
		if err == nil {
			t.Error("Start returned nil, want an error")
		}
	case <-time.After(waitTimeout):
		t.Fatal("Start did not fail on an invalid certificate")
	}
}

func TestStartRequiresHandler(t *testing.T) {
	certs := writeCert(t, t.TempDir(), 1, time.Now())
	if err := (&Server{CertFile: certs.certFile, KeyFile: certs.keyFile}).Start(context.Background()); err == nil {
		t.Error("Start without a handler succeeded, want an error")
	}
}

func TestShutdownLetsInFlightRequestsFinish(t *testing.T) {
	certs := writeCert(t, t.TempDir(), 1, time.Now())
	entered := make(chan struct{})
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(entered)
			<-release
		}
		_, _ = io.WriteString(w, "done")
	})
	run := startServer(t, &Server{Handler: h, CertFile: certs.certFile, KeyFile: certs.keyFile})
	c := client(certs.pool, 0)
	if _, err := get(t, c, run.addr); err != nil {
		t.Fatalf("server not ready: %v", err)
	}

	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.Get("https://" + run.addr + "/slow")
		if err != nil {
			done <- result{err: err}
			return
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		done <- result{body: string(b)}
	}()
	<-entered

	run.cancel()
	close(release)

	select {
	case res := <-done:
		if res.err != nil || res.body != "done" {
			t.Errorf("in-flight request = %q, %v; want \"done\", nil", res.body, res.err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("in-flight request did not finish")
	}
	select {
	case err := <-run.errc:
		if err != nil {
			t.Errorf("Start returned %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Start did not return after shutdown")
	}
}

func TestServerDoesNotNeedLeaderElection(t *testing.T) {
	if (&Server{}).NeedLeaderElection() {
		t.Error("NeedLeaderElection = true, want false")
	}
}
