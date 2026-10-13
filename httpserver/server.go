package httpserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

const (
	defaultShutdownTimeout = 10 * time.Second
	readHeaderTimeout      = 10 * time.Second
)

// Server serves a handler over TLS until its context ends. It reloads the certificate when the
// files change, because cert-manager rotates mounted Secrets in place.
type Server struct {
	Handler http.Handler
	// Listener is used when set; otherwise the server listens on Addr.
	Listener net.Listener
	Logger   *slog.Logger
	Addr     string
	CertFile string
	KeyFile  string
	// ShutdownTimeout bounds how long in-flight requests may finish. Zero means 10s.
	ShutdownTimeout time.Duration
	// Insecure serves plain HTTP and ignores CertFile and KeyFile. For testing and demos only:
	// bearer tokens then travel in clear text and can be replayed by anyone on the network.
	Insecure bool
}

func (s *Server) Start(ctx context.Context) error {
	if s.Handler == nil {
		return errors.New("handler is required")
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	certs := &certReloader{certFile: s.CertFile, keyFile: s.KeyFile, logger: logger}
	if err := certs.load(); err != nil {
		if s.Listener != nil {
			_ = s.Listener.Close()
		}
		return err
	}

	l := s.Listener
	if l == nil {
		var err error
		if l, err = net.Listen("tcp", s.Addr); err != nil {
			return fmt.Errorf("listen on %s: %w", s.Addr, err)
		}
	}

	srv := &http.Server{
		Handler:           s.Handler,
		ReadHeaderTimeout: readHeaderTimeout,
		TLSConfig: &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: certs.getCertificate,
		},
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ServeTLS(l, "", "") }()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	timeout := s.ShutdownTimeout
	if timeout == 0 {
		timeout = defaultShutdownTimeout
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// NeedLeaderElection is false: non-leader replicas must answer too, e.g. with 503 not-leader.
func (s *Server) NeedLeaderElection() bool {
	return false
}

type certReloader struct {
	certModTime time.Time
	keyModTime  time.Time
	cert        *tls.Certificate
	logger      *slog.Logger
	certFile    string
	keyFile     string
	mu          sync.Mutex
}

func (r *certReloader) load() error {
	certMod, keyMod, err := r.modTimes()
	if err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load certificate: %w", err)
	}
	r.cert, r.certModTime, r.keyModTime = &cert, certMod, keyMod
	return nil
}

func (r *certReloader) modTimes() (time.Time, time.Time, error) {
	certInfo, err := os.Stat(r.certFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("stat certificate: %w", err)
	}
	keyInfo, err := os.Stat(r.keyFile)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("stat key: %w", err)
	}
	return certInfo.ModTime(), keyInfo.ModTime(), nil
}

// getCertificate keeps serving the previous certificate when a rotation cannot be loaded,
// e.g. while the files are half-written; it retries once the files change again.
func (r *certReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	certMod, keyMod, err := r.modTimes()
	if err != nil || (certMod.Equal(r.certModTime) && keyMod.Equal(r.keyModTime)) {
		return r.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	r.certModTime, r.keyModTime = certMod, keyMod
	if err != nil {
		r.logger.Error("failed to reload rotated certificate; serving the previous one", "error", err)
		return r.cert, nil
	}
	r.cert = &cert
	return r.cert, nil
}
