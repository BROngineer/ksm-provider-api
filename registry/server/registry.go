package server

import (
	"context"
	"time"
)

type State string

const (
	StateActive    State = "Active"
	StateSuspended State = "Suspended"
)

type Registration struct {
	Name        string
	Endpoint    string
	State       State
	CABundle    []byte
	APIVersions []string
	TTL         time.Duration
	Timeout     time.Duration
}

type Entry struct {
	// ExpiresAt is zero for suspended providers, which never expire.
	ExpiresAt time.Time
	Registration
}

type TransitionKind string

const (
	TransitionRegistered TransitionKind = "Registered"
	TransitionUpdated    TransitionKind = "Updated"
	TransitionSuspended  TransitionKind = "Suspended"
	TransitionResumed    TransitionKind = "Resumed"
	TransitionExpired    TransitionKind = "Expired"
)

// Transition is emitted whenever a provider's reachability or connection details change,
// so the consumer can re-enqueue the provider's ServiceSets. Plain heartbeat refreshes
// emit nothing.
type Transition struct {
	Name string
	Kind TransitionKind
}

type Option func(*Registry)

func WithClock(now func() time.Time) Option {
	return func(*Registry) {}
}

func WithSweepInterval(d time.Duration) Option {
	return func(*Registry) {}
}

// WithTransitionHandler registers a callback invoked synchronously, outside the registry
// lock, for every transition. It must not block for long: heartbeat handling waits on it.
func WithTransitionHandler(h func(Transition)) Option {
	return func(*Registry) {}
}

// Registry is the in-memory provider registry. Its state is rebuilt by heartbeats after a
// restart, so nothing is persisted.
type Registry struct{}

func NewRegistry(opts ...Option) *Registry {
	return &Registry{}
}

func (r *Registry) Register(reg Registration) {}

func (r *Registry) Lookup(name string) (Entry, bool) {
	return Entry{}, false
}

// Sweep removes expired entries and emits TransitionExpired for each.
func (r *Registry) Sweep() {}

// Start sweeps periodically until ctx is done.
func (r *Registry) Start(ctx context.Context) error {
	return nil
}

// NeedLeaderElection keeps expiry on the leader, the only replica that receives heartbeats.
func (r *Registry) NeedLeaderElection() bool {
	return false
}
