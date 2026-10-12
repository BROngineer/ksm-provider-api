package providertest

import (
	"net/http"
	"testing"

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
)

// TestServer serves a Provider through the real provider handler over TLS.
type TestServer struct {
	HTTPClient *http.Client
	URL        string
	// TokenFile holds CallerToken, for clients that read the token from disk.
	TokenFile string
}

func NewTestServer(t *testing.T, p providerserver.Provider) *TestServer {
	t.Helper()
	return &TestServer{}
}

// Client returns a provider client wired to the server.
func (s *TestServer) Client(t *testing.T) *providerclient.Client {
	t.Helper()
	return nil
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
	return &FakeRegistry{}
}
