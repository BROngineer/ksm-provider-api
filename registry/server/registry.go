package server

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"time"
)

const defaultSweepInterval = 5 * time.Second

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
	return func(r *Registry) { r.now = now }
}

func WithSweepInterval(d time.Duration) Option {
	return func(r *Registry) { r.sweepInterval = d }
}

// WithTransitionHandler registers a callback invoked synchronously, outside the registry
// lock, for every transition. It must not block for long: heartbeat handling waits on it.
func WithTransitionHandler(h func(Transition)) Option {
	return func(r *Registry) { r.onTransition = h }
}

// Registry is the in-memory provider registry. Its state is rebuilt by heartbeats after a
// restart, so nothing is persisted.
type Registry struct {
	now           func() time.Time
	onTransition  func(Transition)
	entries       map[string]Entry
	sweepInterval time.Duration
	mu            sync.Mutex
}

func NewRegistry(opts ...Option) *Registry {
	r := &Registry{
		now:           time.Now,
		onTransition:  func(Transition) {},
		entries:       make(map[string]Entry),
		sweepInterval: defaultSweepInterval,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *Registry) Register(reg Registration) {
	reg.CABundle = bytes.Clone(reg.CABundle)
	reg.APIVersions = slices.Clone(reg.APIVersions)

	now := r.now()
	entry := Entry{Registration: reg}
	if reg.State == StateActive {
		entry.ExpiresAt = now.Add(reg.TTL)
	}

	r.mu.Lock()
	prev, existed := r.entries[reg.Name]
	if existed && !prev.alive(now) {
		existed = false
	}
	r.entries[reg.Name] = entry
	r.mu.Unlock()

	if kind, ok := transitionFor(prev, existed, reg); ok {
		r.onTransition(Transition{Name: reg.Name, Kind: kind})
	}
}

func transitionFor(prev Entry, existed bool, next Registration) (TransitionKind, bool) {
	switch {
	case !existed && next.State == StateSuspended:
		return TransitionSuspended, true
	case !existed:
		return TransitionRegistered, true
	case prev.State == StateActive && next.State == StateSuspended:
		return TransitionSuspended, true
	case prev.State == StateSuspended && next.State == StateActive:
		return TransitionResumed, true
	case !sameConnection(prev.Registration, next):
		return TransitionUpdated, true
	default:
		return "", false
	}
}

// sameConnection ignores TTL: it changes how long the entry lives, not how kcm reaches the provider.
func sameConnection(a, b Registration) bool {
	return a.Endpoint == b.Endpoint &&
		bytes.Equal(a.CABundle, b.CABundle) &&
		slices.Equal(a.APIVersions, b.APIVersions) &&
		a.Timeout == b.Timeout
}

func (e Entry) alive(now time.Time) bool {
	return e.ExpiresAt.IsZero() || now.Before(e.ExpiresAt)
}

func (r *Registry) Lookup(name string) (Entry, bool) {
	now := r.now()

	r.mu.Lock()
	entry, ok := r.entries[name]
	r.mu.Unlock()

	if !ok || !entry.alive(now) {
		return Entry{}, false
	}
	entry.CABundle = bytes.Clone(entry.CABundle)
	entry.APIVersions = slices.Clone(entry.APIVersions)
	return entry, true
}

// Sweep removes expired entries and emits TransitionExpired for each.
func (r *Registry) Sweep() {
	now := r.now()

	r.mu.Lock()
	var expired []string
	for name, entry := range r.entries {
		if !entry.alive(now) {
			delete(r.entries, name)
			expired = append(expired, name)
		}
	}
	r.mu.Unlock()

	slices.Sort(expired)
	for _, name := range expired {
		r.onTransition(Transition{Name: name, Kind: TransitionExpired})
	}
}

// Start sweeps periodically until ctx is done.
func (r *Registry) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.Sweep()
		}
	}
}

// NeedLeaderElection keeps expiry on the leader, the only replica that receives heartbeats.
func (r *Registry) NeedLeaderElection() bool {
	return true
}
