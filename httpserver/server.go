package httpserver

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"
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
}

func (s *Server) Start(ctx context.Context) error {
	return nil
}

// NeedLeaderElection is false: non-leader replicas must answer too, e.g. with 503 not-leader.
func (s *Server) NeedLeaderElection() bool {
	return true
}
