package httpclient

import (
	"crypto/tls"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewTrustsCABundle(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	caBundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	c, err := New(caBundle)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET with trusted CA: %v", err)
	}
	_ = resp.Body.Close()

	untrusting, err := New(nil)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if _, err := untrusting.Get(srv.URL); err == nil {
		t.Error("GET without the CA succeeded, want a verification error")
	}
}

func TestNewRejectsBundleWithoutCertificates(t *testing.T) {
	if _, err := New([]byte("not a pem")); err == nil {
		t.Error("New succeeded, want an error")
	}
}

func TestNewRequiresTLS12(t *testing.T) {
	c, err := New(nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := c.Transport.(*http.Transport).TLSClientConfig.MinVersion; got != tls.VersionTLS12 {
		t.Errorf("MinVersion = %x, want TLS 1.2", got)
	}
}
