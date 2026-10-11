package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/brongineer/ksm-provider-api/api/provider"
	"github.com/brongineer/ksm-provider-api/auth"
)

// Provider is the only thing an adapter implements. Deployment failures are results, not
// errors: report them with Applied=false, a Reason and a Message.
type Provider interface {
	DeployServices(ctx context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error)
	DeleteServices(ctx context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error)
}

// maxRequestBytes is generous: requests carry only a ServiceSet key and a generation.
const maxRequestBytes = 16 << 10

var (
	// ErrNotFound means the adapter cannot see the ServiceSet yet; kcm retries.
	ErrNotFound = errors.New("ServiceSet not found")
	// ErrStaleGeneration means the adapter read an older generation than requested; kcm retries.
	ErrStaleGeneration = errors.New("ServiceSet generation is behind the request")
)

type HandlerConfig struct {
	Provider      Provider
	Authenticator auth.Authenticator
	Logger        *slog.Logger
	// AllowedCallers are the usernames allowed to call the provider, normally kcm's ServiceAccount.
	AllowedCallers []string
}

func NewHandler(cfg HandlerConfig) (http.Handler, error) {
	return nil, errors.New("not implemented")
}
