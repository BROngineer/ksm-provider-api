package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/brongineer/ksm-provider-api/api/registry"
	"github.com/brongineer/ksm-provider-api/internal/httpclient"
	"github.com/brongineer/ksm-provider-api/problem"
)

// MinInterval keeps a misconfigured adapter from flooding the registry.
const MinInterval = time.Second

// ttlFactor makes one late heartbeat harmless: an entry survives two missed beats.
const ttlFactor = 3

type Config struct {
	// RegistryURL is the base URL of the kcm registry, e.g. https://kcm-registry.kcm-system.svc:8443.
	RegistryURL string
	// RegistryCABundle verifies the registry's TLS certificate. Nil uses the system roots.
	RegistryCABundle []byte
	// TokenFile is a projected ServiceAccount token for the registry's audience. It is
	// re-read on every heartbeat because the kubelet rotates it.
	TokenFile string

	Name             string
	Endpoint         string
	EndpointCABundle []byte
	APIVersions      []string
	// ProviderTimeout is sent as timeoutSeconds. Zero leaves the registry default.
	ProviderTimeout time.Duration

	Interval time.Duration
	// CallTimeout bounds each heartbeat request. Zero means Interval.
	CallTimeout time.Duration
	// SuspendCheck decides at shutdown whether to report the provider as suspended.
	// Nil never suspends.
	SuspendCheck func(ctx context.Context) (bool, error)

	// HTTPClient replaces the client built from RegistryCABundle.
	HTTPClient *http.Client
	Logger     *slog.Logger
}

type Heartbeater struct {
	client     *registry.Client
	logger     *slog.Logger
	newTicker  func(time.Duration) (<-chan time.Time, func())
	registered chan struct{}
	cfg        Config
	once       sync.Once
}

func NewHeartbeater(cfg Config) (*Heartbeater, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		var err error
		if httpClient, err = httpclient.New(cfg.RegistryCABundle); err != nil {
			return nil, fmt.Errorf("registry CA bundle: %w", err)
		}
	}
	c, err := registry.NewClient(cfg.RegistryURL, registry.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create registry client: %w", err)
	}

	if cfg.CallTimeout == 0 {
		cfg.CallTimeout = cfg.Interval
	}
	cfg.EndpointCABundle = bytes.Clone(cfg.EndpointCABundle)
	cfg.APIVersions = slices.Clone(cfg.APIVersions)

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Heartbeater{
		client:     c,
		logger:     logger.With("provider", cfg.Name),
		newTicker:  newRealTicker,
		registered: make(chan struct{}),
		cfg:        cfg,
	}, nil
}

func validate(cfg Config) error {
	var errs []error
	if u, err := url.Parse(cfg.RegistryURL); err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, errors.New("registry URL must be absolute"))
	}
	if cfg.TokenFile == "" {
		errs = append(errs, errors.New("token file is required"))
	}
	if cfg.Name == "" {
		errs = append(errs, errors.New("name is required"))
	}
	if cfg.Endpoint == "" {
		errs = append(errs, errors.New("endpoint is required"))
	}
	if len(cfg.APIVersions) == 0 {
		errs = append(errs, errors.New("at least one API version is required"))
	}
	if cfg.Interval < MinInterval {
		errs = append(errs, fmt.Errorf("interval must be at least %s", MinInterval))
	}
	if cfg.ProviderTimeout < 0 {
		errs = append(errs, errors.New("provider timeout must not be negative"))
	}
	return errors.Join(errs...)
}

func newRealTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Start heartbeats until ctx is done, then sends a final Suspended heartbeat if SuspendCheck
// says so.
func (h *Heartbeater) Start(ctx context.Context) error {
	ticks, stop := h.newTicker(h.cfg.Interval)
	defer stop()

	h.beat(ctx, registry.RegisterProviderRequestStateActive)
	for {
		select {
		case <-ctx.Done():
			h.shutdown()
			return nil
		case <-ticks:
			h.beat(ctx, registry.RegisterProviderRequestStateActive)
		}
	}
}

func (h *Heartbeater) beat(ctx context.Context, state registry.RegisterProviderRequestState) {
	callCtx, cancel := context.WithTimeout(ctx, h.cfg.CallTimeout)
	defer cancel()
	if err := h.send(callCtx, state); err != nil {
		h.logger.Warn("heartbeat failed", "state", state, "error", err)
		return
	}
	h.once.Do(func() { close(h.registered) })
}

// shutdown runs after the manager's context is cancelled, so it needs a context of its own.
func (h *Heartbeater) shutdown() {
	if h.cfg.SuspendCheck == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.CallTimeout)
	defer cancel()

	suspended, err := h.cfg.SuspendCheck(ctx)
	if err != nil {
		h.logger.Warn("suspend check failed; letting the registration expire", "error", err)
		return
	}
	if !suspended {
		return
	}
	if err := h.send(ctx, registry.RegisterProviderRequestStateSuspended); err != nil {
		h.logger.Warn("suspend heartbeat failed; letting the registration expire", "error", err)
	}
}

func (h *Heartbeater) send(ctx context.Context, state registry.RegisterProviderRequestState) error {
	token, err := os.ReadFile(h.cfg.TokenFile)
	if err != nil {
		return fmt.Errorf("read token: %w", err)
	}
	bearer := "Bearer " + strings.TrimSpace(string(token))

	resp, err := h.client.RegisterKSMProvider(ctx, h.request(state), func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", bearer)
		return nil
	})
	if err != nil {
		return fmt.Errorf("send heartbeat: %w", err)
	}
	// The status is all that matters once read; a close error has nothing left to affect.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return responseError(resp)
}

func (h *Heartbeater) request(state registry.RegisterProviderRequestState) registry.RegisterProviderRequest {
	req := registry.RegisterProviderRequest{
		Name:        h.cfg.Name,
		Endpoint:    h.cfg.Endpoint,
		State:       state,
		ApiVersions: h.cfg.APIVersions,
		TtlSeconds:  ceilSeconds(ttlFactor * h.cfg.Interval),
	}
	if len(h.cfg.EndpointCABundle) > 0 {
		req.CaBundle = &h.cfg.EndpointCABundle
	}
	if h.cfg.ProviderTimeout > 0 {
		req.TimeoutSeconds = new(ceilSeconds(h.cfg.ProviderTimeout))
	}
	return req
}

func ceilSeconds(d time.Duration) int {
	return int(math.Ceil(d.Seconds()))
}

func responseError(resp *http.Response) error {
	p := problem.FromResponse(resp)
	return fmt.Errorf("registry answered %d (%s): %w", p.Status, p.Type, p)
}

// NeedLeaderElection keeps a single replica heartbeating, so replicas never overwrite each other.
func (h *Heartbeater) NeedLeaderElection() bool {
	return true
}

// Registered is closed after the first accepted heartbeat. Nudges sent before it would hit
// a registry that does not know the provider yet.
func (h *Heartbeater) Registered() <-chan struct{} {
	return h.registered
}

// DeploymentScaledToZero distinguishes a scale-down from a rollout, eviction or drain, which
// also stop the pod but must not suspend the provider.
func DeploymentScaledToZero(cs kubernetes.Interface, key types.NamespacedName) func(ctx context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		d, err := cs.AppsV1().Deployments(key.Namespace).Get(ctx, key.Name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("get deployment %s: %w", key, err)
		}
		return d.Spec.Replicas != nil && *d.Spec.Replicas == 0, nil
	}
}
