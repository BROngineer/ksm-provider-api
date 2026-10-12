package server

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	t  time.Time
	mu sync.Mutex
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

type recorder struct {
	got []Transition
	mu  sync.Mutex
}

func (r *recorder) handle(t Transition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, t)
}

func (r *recorder) transitions() []Transition {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

func newTestRegistry(t *testing.T, opts ...Option) (*Registry, *fakeClock, *recorder) {
	t.Helper()
	clock := newFakeClock()
	rec := &recorder{}
	opts = append([]Option{WithClock(clock.now), WithTransitionHandler(rec.handle)}, opts...)
	return NewRegistry(opts...), clock, rec
}

func activeRegistration(name string) Registration {
	return Registration{
		Name:        name,
		Endpoint:    "https://ksm-provider-sveltos.projectsveltos.svc:9443",
		State:       StateActive,
		CABundle:    []byte("ca"),
		APIVersions: []string{"v1"},
		TTL:         30 * time.Second,
		Timeout:     30 * time.Second,
	}
}

func assertTransitions(t *testing.T, rec *recorder, want ...Transition) {
	t.Helper()
	if got := rec.transitions(); !slices.Equal(got, want) {
		t.Errorf("transitions = %v, want %v", got, want)
	}
}

func TestRegisterNewActiveProvider(t *testing.T) {
	r, clock, rec := newTestRegistry(t)
	reg := activeRegistration("sveltos")

	r.Register(reg)

	got, ok := r.Lookup("sveltos")
	if !ok {
		t.Fatal("Lookup: provider not found")
	}
	if got.Endpoint != reg.Endpoint || got.State != StateActive || got.Timeout != reg.Timeout {
		t.Errorf("entry = %+v, want registration %+v", got, reg)
	}
	if want := clock.now().Add(reg.TTL); !got.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want)
	}
	assertTransitions(t, rec, Transition{Name: "sveltos", Kind: TransitionRegistered})
}

func TestRefreshWithSameDetailsExtendsExpiryWithoutTransition(t *testing.T) {
	r, clock, rec := newTestRegistry(t)
	reg := activeRegistration("sveltos")
	r.Register(reg)

	clock.advance(20 * time.Second)
	r.Register(reg)

	got, _ := r.Lookup("sveltos")
	if want := clock.now().Add(reg.TTL); !got.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want)
	}
	assertTransitions(t, rec, Transition{Name: "sveltos", Kind: TransitionRegistered})
}

func TestChangedConnectionDetailsEmitUpdated(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Registration)
	}{
		{"endpoint", func(r *Registration) { r.Endpoint = "https://other.svc:9443" }},
		{"caBundle", func(r *Registration) { r.CABundle = []byte("rotated-ca") }},
		{"apiVersions", func(r *Registration) { r.APIVersions = []string{"v1", "v2"} }},
		{"timeout", func(r *Registration) { r.Timeout = time.Minute }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _, rec := newTestRegistry(t)
			reg := activeRegistration("sveltos")
			r.Register(reg)

			changed := activeRegistration("sveltos")
			tt.mutate(&changed)
			r.Register(changed)

			assertTransitions(t, rec,
				Transition{Name: "sveltos", Kind: TransitionRegistered},
				Transition{Name: "sveltos", Kind: TransitionUpdated},
			)
		})
	}
}

func TestTTLChangeAloneIsNotATransition(t *testing.T) {
	r, _, rec := newTestRegistry(t)
	r.Register(activeRegistration("sveltos"))

	changed := activeRegistration("sveltos")
	changed.TTL = time.Minute
	r.Register(changed)

	assertTransitions(t, rec, Transition{Name: "sveltos", Kind: TransitionRegistered})
}

func TestSuspendedProviderNeverExpires(t *testing.T) {
	r, clock, rec := newTestRegistry(t)
	r.Register(activeRegistration("sveltos"))

	suspended := activeRegistration("sveltos")
	suspended.State = StateSuspended
	r.Register(suspended)
	clock.advance(24 * time.Hour)
	r.Sweep()

	got, ok := r.Lookup("sveltos")
	if !ok {
		t.Fatal("suspended provider expired")
	}
	if got.State != StateSuspended || !got.ExpiresAt.IsZero() {
		t.Errorf("entry = %+v, want suspended with zero ExpiresAt", got)
	}
	assertTransitions(t, rec,
		Transition{Name: "sveltos", Kind: TransitionRegistered},
		Transition{Name: "sveltos", Kind: TransitionSuspended},
	)
}

func TestSuspendedRefreshIsNotATransition(t *testing.T) {
	r, _, rec := newTestRegistry(t)
	suspended := activeRegistration("sveltos")
	suspended.State = StateSuspended

	r.Register(suspended)
	r.Register(suspended)

	assertTransitions(t, rec, Transition{Name: "sveltos", Kind: TransitionSuspended})
}

func TestActiveAfterSuspendedResumes(t *testing.T) {
	r, clock, rec := newTestRegistry(t)
	suspended := activeRegistration("sveltos")
	suspended.State = StateSuspended
	r.Register(suspended)

	reg := activeRegistration("sveltos")
	r.Register(reg)

	got, _ := r.Lookup("sveltos")
	if got.State != StateActive {
		t.Errorf("State = %q, want %q", got.State, StateActive)
	}
	if want := clock.now().Add(reg.TTL); !got.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, want)
	}
	assertTransitions(t, rec,
		Transition{Name: "sveltos", Kind: TransitionSuspended},
		Transition{Name: "sveltos", Kind: TransitionResumed},
	)
}

func TestActiveProviderExpiresAfterTTL(t *testing.T) {
	r, clock, rec := newTestRegistry(t)
	r.Register(activeRegistration("sveltos"))

	clock.advance(29 * time.Second)
	if _, ok := r.Lookup("sveltos"); !ok {
		t.Fatal("provider expired before its TTL")
	}

	clock.advance(time.Second)
	if _, ok := r.Lookup("sveltos"); ok {
		t.Error("Lookup found a provider past its TTL")
	}

	r.Sweep()
	r.Sweep()
	assertTransitions(t, rec,
		Transition{Name: "sveltos", Kind: TransitionRegistered},
		Transition{Name: "sveltos", Kind: TransitionExpired},
	)
}

func TestRegisterAfterUnsweptExpiryIsRegistered(t *testing.T) {
	r, clock, rec := newTestRegistry(t)
	r.Register(activeRegistration("sveltos"))

	clock.advance(time.Minute)
	r.Register(activeRegistration("sveltos"))

	if _, ok := r.Lookup("sveltos"); !ok {
		t.Fatal("re-registered provider not found")
	}
	assertTransitions(t, rec,
		Transition{Name: "sveltos", Kind: TransitionRegistered},
		Transition{Name: "sveltos", Kind: TransitionRegistered},
	)
}

func TestLookupUnknownProvider(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	if _, ok := r.Lookup("missing"); ok {
		t.Error("Lookup found an unregistered provider")
	}
}

func TestRegistryIsolatesCallerSlices(t *testing.T) {
	r, _, _ := newTestRegistry(t)
	reg := activeRegistration("sveltos")
	r.Register(reg)

	reg.CABundle[0] = 'X'
	reg.APIVersions[0] = "mutated"
	got, _ := r.Lookup("sveltos")
	if string(got.CABundle) != "ca" || got.APIVersions[0] != "v1" {
		t.Errorf("stored entry changed through the registration slices: %+v", got)
	}

	got.CABundle[0] = 'Y'
	got.APIVersions[0] = "mutated"
	again, _ := r.Lookup("sveltos")
	if string(again.CABundle) != "ca" || again.APIVersions[0] != "v1" {
		t.Errorf("stored entry changed through the lookup slices: %+v", again)
	}
}

func TestTransitionHandlerMayCallBackIntoRegistry(t *testing.T) {
	clock := newFakeClock()
	var r *Registry
	looked := make(chan bool, 1)
	r = NewRegistry(WithClock(clock.now), WithTransitionHandler(func(tr Transition) {
		_, ok := r.Lookup(tr.Name)
		looked <- ok
	}))

	done := make(chan struct{})
	go func() {
		r.Register(activeRegistration("sveltos"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Register deadlocked when the handler called Lookup")
	}
	if !<-looked {
		t.Error("handler did not see the registered provider")
	}
}

func TestStartSweepsPeriodically(t *testing.T) {
	r, clock, rec := newTestRegistry(t, WithSweepInterval(10*time.Millisecond))
	r.Register(activeRegistration("sveltos"))
	clock.advance(time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- r.Start(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(rec.transitions(), Transition{Name: "sveltos", Kind: TransitionExpired}) {
		if time.Now().After(deadline) {
			t.Fatal("Start did not sweep the expired provider")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Start returned %v, want nil after cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

func TestRegistryNeedsLeaderElection(t *testing.T) {
	if !NewRegistry().NeedLeaderElection() {
		t.Error("NeedLeaderElection = false, want true")
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	r, clock, _ := newTestRegistry(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			name := fmt.Sprintf("provider-%d", i%3)
			for range 100 {
				r.Register(activeRegistration(name))
				r.Lookup(name)
				clock.advance(time.Second)
				r.Sweep()
			}
		})
	}
	wg.Wait()
}
