package httpserver

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func plainGet(t *testing.T, addr string) (*http.Response, error) {
	t.Helper()
	c := &http.Client{Timeout: waitTimeout}
	var resp *http.Response
	var err error
	deadline := time.Now().Add(waitTimeout)
	for {
		resp, err = c.Get("http://" + addr + "/")
		if err == nil || time.Now().After(deadline) {
			return resp, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInsecureServesPlainHTTP(t *testing.T) {
	run := startServer(t, &Server{Handler: okHandler(), Insecure: true})

	resp, err := plainGet(t, run.addr)
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

func TestInsecureIgnoresCertificateFiles(t *testing.T) {
	run := startServer(t, &Server{
		Handler:  okHandler(),
		Insecure: true,
		CertFile: "/does/not/exist.crt",
		KeyFile:  "/does/not/exist.key",
	})

	if _, err := plainGet(t, run.addr); err != nil {
		t.Fatalf("GET with missing cert files in insecure mode: %v", err)
	}
	run.stop(t)
}

func TestInsecureWarnsAtStartup(t *testing.T) {
	var logs syncBuffer
	run := startServer(t, &Server{
		Handler:  okHandler(),
		Insecure: true,
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if _, err := plainGet(t, run.addr); err != nil {
		t.Fatalf("server not ready: %v", err)
	}
	run.stop(t)

	if got := logs.String(); !strings.Contains(got, "level=WARN") || !strings.Contains(got, "without TLS") {
		t.Errorf("logs = %q, want a warning about serving without TLS", got)
	}
}

func TestSecureModeDoesNotWarn(t *testing.T) {
	var logs syncBuffer
	certs := writeCert(t, t.TempDir(), 1, time.Now())
	run := startServer(t, &Server{
		Handler:  okHandler(),
		CertFile: certs.certFile,
		KeyFile:  certs.keyFile,
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if _, err := get(t, client(certs.pool, 0), run.addr); err != nil {
		t.Fatalf("server not ready: %v", err)
	}
	run.stop(t)

	if strings.Contains(logs.String(), "without TLS") {
		t.Errorf("TLS mode logged the insecure warning: %q", logs.String())
	}
}
