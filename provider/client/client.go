package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/brongineer/ksm-provider-api/api/provider"
	"github.com/brongineer/ksm-provider-api/internal/httpclient"
	"github.com/brongineer/ksm-provider-api/internal/schema"
	"github.com/brongineer/ksm-provider-api/problem"
)

// maxResultBytes leaves room for ServiceSets with many services.
const maxResultBytes = 4 << 20

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

type Client struct {
	api          *provider.Client
	deployResult *openapi3.Schema
	deleteResult *openapi3.Schema
	tokenFile    string
	timeout      time.Duration
}

func New(cfg Config) (*Client, error) {
	if u, err := url.Parse(cfg.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, errors.New("endpoint must be an absolute URL")
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("token file is required")
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("timeout must be positive")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		var err error
		if httpClient, err = httpclient.New(cfg.CABundle); err != nil {
			return nil, fmt.Errorf("provider CA bundle: %w", err)
		}
	}
	api, err := provider.NewClient(cfg.Endpoint, provider.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create provider client: %w", err)
	}
	deployResult, err := schema.Load(provider.GetSpec, "DeployServicesResult")
	if err != nil {
		return nil, err
	}
	deleteResult, err := schema.Load(provider.GetSpec, "DeleteServicesResult")
	if err != nil {
		return nil, err
	}

	return &Client{
		api:          api,
		deployResult: deployResult,
		deleteResult: deleteResult,
		tokenFile:    cfg.TokenFile,
		timeout:      cfg.Timeout,
	}, nil
}

type sendFunc func(ctx context.Context, editor provider.RequestEditorFn) (*http.Response, error)

func (c *Client) DeployServices(ctx context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error) {
	var res provider.DeployServicesResult
	send := func(ctx context.Context, editor provider.RequestEditorFn) (*http.Response, error) {
		return c.api.DeployServices(ctx, req, editor)
	}
	if err := c.call(ctx, send, c.deployResult, &res); err != nil {
		return provider.DeployServicesResult{}, err
	}
	// The schema cannot express this rule, and kcm needs both to explain the failure.
	if !res.Applied && (res.Reason == nil || res.Message == nil || *res.Message == "") {
		return provider.DeployServicesResult{}, fmt.Errorf("%w: applied=false requires both reason and message", ErrInvalidResult)
	}
	return res, nil
}

func (c *Client) DeleteServices(ctx context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error) {
	var res provider.DeleteServicesResult
	send := func(ctx context.Context, editor provider.RequestEditorFn) (*http.Response, error) {
		return c.api.DeleteServices(ctx, req, editor)
	}
	if err := c.call(ctx, send, c.deleteResult, &res); err != nil {
		return provider.DeleteServicesResult{}, err
	}
	return res, nil
}

// call validates 200 bodies itself: kcm cannot assume a third-party adapter uses this
// module's server.
func (c *Client) call(ctx context.Context, send sendFunc, s *openapi3.Schema, out any) error {
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	bearer := "Bearer " + strings.TrimSpace(string(token))

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	resp, err := send(ctx, func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", bearer)
		return nil
	})
	if err != nil {
		return fmt.Errorf("call provider: %w", err)
	}
	// Everything needed has been read by the time the body is closed.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return &ProblemError{Problem: problem.FromResponse(resp)}
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResultBytes))
	if err != nil {
		return fmt.Errorf("read provider result: %w", err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("%w: not valid JSON", ErrInvalidResult)
	}
	if fieldErrs := schema.Validate(s, doc); fieldErrs != nil {
		return fmt.Errorf("%w: %v", ErrInvalidResult, fieldErrs)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidResult, err)
	}
	return nil
}
