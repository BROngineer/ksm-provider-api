package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/brongineer/ksm-provider-api/auth"
)

// maxRequestBytes leaves room for a CA bundle of a few certificates.
const maxRequestBytes = 64 << 10

const defaultTimeout = 30

type HandlerConfig struct {
	Registry      *Registry
	Authenticator auth.Authenticator
	// Allowed maps a provider name to the only ServiceAccount username that may register it.
	Allowed map[string]string
	// IsLeader reports whether this replica holds the registry. Nil means always.
	IsLeader func() bool
	Logger   *slog.Logger
}

func NewHandler(cfg HandlerConfig) (http.Handler, error) {
	return nil, errors.New("not implemented")
}

// LeaderFromElected adapts controller-runtime's Manager.Elected channel.
func LeaderFromElected(elected <-chan struct{}) func() bool {
	return func() bool { return false }
}
