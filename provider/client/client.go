package client

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/brongineer/ksm-provider-api/api/provider"
	"github.com/brongineer/ksm-provider-api/problem"
)

// ErrInvalidResult marks a 200 response that breaks the contract: an adapter bug, not a
// transient failure.
var ErrInvalidResult = errors.New("provider returned an invalid result")

// ProblemError is returned for every non-2xx response, whoever produced it.
type ProblemError struct {
	problem.Problem
}

type Config struct {
	// Endpoint is the provider's base URL as registered in the registry.
	Endpoint string
	CABundle []byte
	// TokenFile is a projected ServiceAccount token for the provider audience. It is re-read
	// on every call because the kubelet rotates it.
	TokenFile string
	// Timeout bounds each call.
	Timeout    time.Duration
	HTTPClient *http.Client
}

type Client struct{}

func New(cfg Config) (*Client, error) {
	return nil, errors.New("not implemented")
}

func (c *Client) DeployServices(ctx context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error) {
	return provider.DeployServicesResult{}, nil
}

func (c *Client) DeleteServices(ctx context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error) {
	return provider.DeleteServicesResult{}, nil
}
