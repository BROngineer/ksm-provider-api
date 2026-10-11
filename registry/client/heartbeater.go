package client

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// MinInterval keeps a misconfigured adapter from flooding the registry.
const MinInterval = time.Second

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
	newTicker func(time.Duration) (<-chan time.Time, func())
}

func NewHeartbeater(cfg Config) (*Heartbeater, error) {
	return nil, errors.New("not implemented")
}

// Start heartbeats until ctx is done, then sends a final Suspended heartbeat if SuspendCheck
// says so.
func (h *Heartbeater) Start(ctx context.Context) error {
	return nil
}

// NeedLeaderElection keeps a single replica heartbeating, so replicas never overwrite each other.
func (h *Heartbeater) NeedLeaderElection() bool {
	return false
}

// Registered is closed after the first accepted heartbeat. Nudges sent before it would hit
// a registry that does not know the provider yet.
func (h *Heartbeater) Registered() <-chan struct{} {
	return nil
}

// DeploymentScaledToZero distinguishes a scale-down from a rollout, eviction or drain, which
// also stop the pod but must not suspend the provider.
func DeploymentScaledToZero(cs kubernetes.Interface, key types.NamespacedName) func(ctx context.Context) (bool, error) {
	return func(context.Context) (bool, error) { return false, nil }
}
