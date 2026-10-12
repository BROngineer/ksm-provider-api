package providertest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brongineer/ksm-provider-api/auth"
	providerclient "github.com/brongineer/ksm-provider-api/provider/client"
	providerserver "github.com/brongineer/ksm-provider-api/provider/server"
	registryserver "github.com/brongineer/ksm-provider-api/registry/server"
)

const (
	// CallerToken is the only token TestServer accepts.
	CallerToken = "providertest-kcm-token"
	// CallerUsername is the identity TestServer authorizes, standing in for kcm's ServiceAccount.
	CallerUsername = "system:serviceaccount:kcm-system:kcm-controller-manager"

	// ProviderToken is the only token FakeRegistry accepts.
	ProviderToken = "providertest-provider-token"
	// ProviderUsername is the identity FakeRegistry allows to register any of its names.
	ProviderUsername = "system:serviceaccount:providertest:ksm-provider"

	clientTimeout = 10 * time.Second
)

// staticAuthenticator replaces TokenReview, which needs an API server.
type staticAuthenticator map[string]string

func (a staticAuthenticator) Authenticate(_ context.Context, token string) (string, error) {
	if user, ok := a[token]; ok {
		return user, nil
	}
	return "", auth.ErrUnauthenticated
}

// TestServer serves a Provider through the real provider handler over TLS.
type TestServer struct {
	HTTPClient *http.Client
	URL        string
	// TokenFile holds CallerToken, for clients that read the token from disk.
	TokenFile string
}

func NewTestServer(t *testing.T, p providerserver.Provider) *TestServer {
	t.Helper()
	h, err := providerserver.NewHandler(providerserver.HandlerConfig{
		Provider:       p,
		Authenticator:  staticAuthenticator{CallerToken: CallerUsername},
		AllowedCallers: []string{CallerUsername},
	})
	if err != nil {
		t.Fatalf("providertest: build provider handler: %v", err)
	}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return &TestServer{HTTPClient: srv.Client(), URL: srv.URL, TokenFile: writeTokenFile(t, CallerToken)}
}

// Client returns a provider client wired to the server.
func (s *TestServer) Client(t *testing.T) *providerclient.Client {
	t.Helper()
	c, err := providerclient.New(providerclient.Config{
		Endpoint:   s.URL,
		TokenFile:  s.TokenFile,
		Timeout:    clientTimeout,
		HTTPClient: s.HTTPClient,
	})
	if err != nil {
		t.Fatalf("providertest: build provider client: %v", err)
	}
	return c
}

// FakeRegistry serves the real registry handler over TLS, so adapter tests can run a real
// Heartbeater against it.
type FakeRegistry struct {
	Registry   *registryserver.Registry
	HTTPClient *http.Client
	URL        string
	// TokenFile holds ProviderToken, for the Heartbeater's TokenFile.
	TokenFile string
}

// NewFakeRegistry allows ProviderUsername to register each of names.
func NewFakeRegistry(t *testing.T, names ...string) *FakeRegistry {
	t.Helper()
	allowed := make(map[string]string, len(names))
	for _, name := range names {
		allowed[name] = ProviderUsername
	}
	reg := registryserver.NewRegistry()
	h, err := registryserver.NewHandler(registryserver.HandlerConfig{
		Registry:      reg,
		Authenticator: staticAuthenticator{ProviderToken: ProviderUsername},
		Allowed:       allowed,
	})
	if err != nil {
		t.Fatalf("providertest: build registry handler: %v", err)
	}
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return &FakeRegistry{Registry: reg, HTTPClient: srv.Client(), URL: srv.URL, TokenFile: writeTokenFile(t, ProviderToken)}
}

func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("providertest: write token file: %v", err)
	}
	return path
}
