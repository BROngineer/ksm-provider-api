package providertest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/brongineer/ksm-provider-api/api/provider"
	providerserver "github.com/brongineer/ksm-provider-api/provider/server"
)

// memoryProvider follows the contract; its fields switch individual violations on.
type memoryProvider struct {
	sets map[string]*memorySet

	reportFailureAsError bool
	ignoreGeneration     bool
	neverNotFound        bool
	neverStale           bool
	neverDeleted         bool
	failSecondDeploy     bool

	deploys int
	mu      sync.Mutex
}

type memorySet struct {
	generation  int64
	failing     bool
	deleteCalls int
	deleted     bool
}

func newMemoryProvider() *memoryProvider {
	return &memoryProvider{sets: map[string]*memorySet{}}
}

func (m *memoryProvider) add(name string, generation int64, failing bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sets["ns/"+name] = &memorySet{generation: generation, failing: failing}
}

func (m *memoryProvider) DeployServices(_ context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deploys++
	if m.failSecondDeploy && m.deploys > 1 {
		return provider.DeployServicesResult{}, errors.New("already deployed")
	}

	set, ok := m.sets[req.Namespace+"/"+req.Name]
	if !ok || set.deleted {
		if m.neverNotFound {
			return provider.DeployServicesResult{ObservedGeneration: req.Generation, Applied: true, Collected: true}, nil
		}
		return provider.DeployServicesResult{}, fmt.Errorf("get %s: %w", req.Name, providerserver.ErrNotFound)
	}
	if req.Generation > set.generation && !m.neverStale {
		return provider.DeployServicesResult{}, fmt.Errorf("read generation %d: %w", set.generation, providerserver.ErrStaleGeneration)
	}

	observed := set.generation
	if m.ignoreGeneration {
		observed = 0
	}
	if set.failing {
		if m.reportFailureAsError {
			return provider.DeployServicesResult{}, errors.New("template not found")
		}
		return provider.DeployServicesResult{
			ObservedGeneration: observed,
			Reason:             new(provider.DeployServicesResultReasonInvalidInput),
			Message:            new("template not found"),
		}, nil
	}
	return provider.DeployServicesResult{
		ObservedGeneration: observed,
		Applied:            true,
		Collected:          true,
		Services: []provider.ServiceState{{
			Name: "svc", Namespace: "ns", Template: "tpl",
			Type: provider.ServiceStateTypeHelm, State: provider.ServiceStateStateDeployed,
		}},
	}, nil
}

// DeleteServices takes two calls to finish, so the contract's polling is exercised.
func (m *memoryProvider) DeleteServices(_ context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.sets[req.Namespace+"/"+req.Name]
	if !ok {
		return provider.DeleteServicesResult{Done: true}, nil
	}
	if m.neverDeleted {
		return provider.DeleteServicesResult{Done: false}, nil
	}
	set.deleteCalls++
	set.deleted = set.deleteCalls >= 2
	return provider.DeleteServicesResult{Done: set.deleted}, nil
}

func scenariosFor(newProvider func() *memoryProvider) Scenarios {
	var current *memoryProvider
	return Scenarios{
		NewProvider: func(*testing.T) providerserver.Provider {
			current = newProvider()
			return current
		},
		Deployable: func(*testing.T) provider.DeployServicesRequest {
			current.add("deployable", 3, false)
			return provider.DeployServicesRequest{Name: "deployable", Namespace: "ns", Generation: 3}
		},
		Failing: func(*testing.T) provider.DeployServicesRequest {
			current.add("failing", 1, true)
			return provider.DeployServicesRequest{Name: "failing", Namespace: "ns", Generation: 1}
		},
		Missing: func(*testing.T) provider.DeployServicesRequest {
			return provider.DeployServicesRequest{Name: "missing", Namespace: "ns", Generation: 1}
		},
		Stale: func(*testing.T) provider.DeployServicesRequest {
			current.add("stale", 2, false)
			return provider.DeployServicesRequest{Name: "stale", Namespace: "ns", Generation: 3}
		},
		DeleteTimeout: 2 * time.Second,
	}
}

func findCheck(t *testing.T, name string) check {
	t.Helper()
	for _, c := range checks {
		if c.name == name {
			return c
		}
	}
	t.Fatalf("no contract check named %q", name)
	return check{}
}

func runCheck(t *testing.T, name string, s Scenarios) error {
	t.Helper()
	c := findCheck(t, name)
	srv := NewTestServer(t, s.NewProvider(t))
	return c.run(t, srv.Client(t), s)
}

func TestRunContractPassesForConformingProvider(t *testing.T) {
	RunContract(t, scenariosFor(newMemoryProvider))
}

func TestChecksPassForConformingProvider(t *testing.T) {
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if err := runCheck(t, c.name, scenariosFor(newMemoryProvider)); err != nil {
				t.Errorf("check failed for a conforming provider: %v", err)
			}
		})
	}
}

func TestChecksDetectViolations(t *testing.T) {
	tests := []struct {
		check   string
		violate func(*memoryProvider)
	}{
		{"deployable", func(m *memoryProvider) { m.ignoreGeneration = true }},
		{"failing", func(m *memoryProvider) { m.reportFailureAsError = true }},
		{"failing", func(m *memoryProvider) { m.ignoreGeneration = true }},
		{"missing", func(m *memoryProvider) { m.neverNotFound = true }},
		{"stale", func(m *memoryProvider) { m.neverStale = true }},
		{"idempotent", func(m *memoryProvider) { m.failSecondDeploy = true }},
		{"delete", func(m *memoryProvider) { m.neverDeleted = true }},
	}
	for _, tt := range tests {
		t.Run(tt.check, func(t *testing.T) {
			s := scenariosFor(func() *memoryProvider {
				m := newMemoryProvider()
				tt.violate(m)
				return m
			})
			s.DeleteTimeout = 300 * time.Millisecond

			if err := runCheck(t, tt.check, s); err == nil {
				t.Error("check passed for a violating provider, want an error")
			}
		})
	}
}

func TestChecksCoverTheContract(t *testing.T) {
	want := []string{"deployable", "failing", "missing", "stale", "idempotent", "delete"}
	for _, name := range want {
		findCheck(t, name)
	}
	if len(checks) != len(want) {
		t.Errorf("found %d checks, want %d", len(checks), len(want))
	}
}

func TestOptionalScenariosAreSkippedWhenUnset(t *testing.T) {
	s := scenariosFor(newMemoryProvider)
	s.Failing, s.Missing, s.Stale = nil, nil, nil

	for name, wantRun := range map[string]bool{
		"deployable": true, "idempotent": true, "delete": true,
		"failing": false, "missing": false, "stale": false,
	} {
		if got := findCheck(t, name).needs(s); got != wantRun {
			t.Errorf("check %q runs = %v with optional scenarios unset, want %v", name, got, wantRun)
		}
	}
}
