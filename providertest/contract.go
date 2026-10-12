package providertest

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/brongineer/ksm-provider-api/api/provider"
	"github.com/brongineer/ksm-provider-api/problem"
	providerclient "github.com/brongineer/ksm-provider-api/provider/client"
	providerserver "github.com/brongineer/ksm-provider-api/provider/server"
)

const (
	defaultDeleteTimeout = 30 * time.Second
	deletePollInterval   = 100 * time.Millisecond
)

// Scenarios lets an adapter author drive their provider into the situations the contract
// covers. Each func prepares the environment (e.g. creates a ServiceSet in envtest) and
// returns the request kcm would send. Scenarios left nil are skipped.
type Scenarios struct {
	// NewProvider builds a fresh provider for each contract check. Required.
	NewProvider func(t *testing.T) providerserver.Provider
	// Deployable returns a request for a ServiceSet the provider can deploy. Required.
	Deployable func(t *testing.T) provider.DeployServicesRequest
	// Failing returns a request whose deployment fails, e.g. one referencing a missing template.
	Failing func(t *testing.T) provider.DeployServicesRequest
	// Missing returns a request for a ServiceSet the provider cannot see.
	Missing func(t *testing.T) provider.DeployServicesRequest
	// Stale returns a request whose generation is ahead of what the provider can read.
	Stale func(t *testing.T) provider.DeployServicesRequest
	// DeleteTimeout bounds how long deleting a deployed ServiceSet may take. Zero means 30s.
	DeleteTimeout time.Duration
}

type check struct {
	name  string
	needs func(Scenarios) bool
	run   func(t *testing.T, c *providerclient.Client, s Scenarios) error
}

var checks = []check{
	{name: "deployable", needs: always, run: checkDeployable},
	{name: "failing", needs: func(s Scenarios) bool { return s.Failing != nil }, run: checkFailing},
	{name: "missing", needs: func(s Scenarios) bool { return s.Missing != nil }, run: checkMissing},
	{name: "stale", needs: func(s Scenarios) bool { return s.Stale != nil }, run: checkStale},
	{name: "idempotent", needs: always, run: checkIdempotent},
	{name: "delete", needs: always, run: checkDelete},
}

func always(Scenarios) bool { return true }

// RunContract verifies a provider against the contract kcm relies on, through the real HTTP
// server and client, so request and result validation are exercised too.
func RunContract(t *testing.T, s Scenarios) {
	t.Helper()
	if s.NewProvider == nil || s.Deployable == nil {
		t.Fatal("providertest: Scenarios.NewProvider and Scenarios.Deployable are required")
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if !c.needs(s) {
				t.Skip("scenario not provided")
			}
			srv := NewTestServer(t, s.NewProvider(t))
			if err := c.run(t, srv.Client(t), s); err != nil {
				t.Error(err)
			}
		})
	}
}

func checkDeployable(t *testing.T, c *providerclient.Client, s Scenarios) error {
	req := s.Deployable(t)
	res, err := c.DeployServices(t.Context(), req)
	if err != nil {
		return fmt.Errorf("deploying a deployable ServiceSet failed: %w", err)
	}
	if !res.Applied {
		return fmt.Errorf("deployable ServiceSet reported applied=false: %s", describeFailure(res))
	}
	return checkObservedGeneration(req, res)
}

func checkFailing(t *testing.T, c *providerclient.Client, s Scenarios) error {
	req := s.Failing(t)
	res, err := c.DeployServices(t.Context(), req)
	if err != nil {
		return fmt.Errorf("deployment failures must be returned as applied=false results, not errors: %w", err)
	}
	if res.Applied {
		return errors.New("failing ServiceSet reported applied=true")
	}
	return checkObservedGeneration(req, res)
}

func checkMissing(t *testing.T, c *providerclient.Client, s Scenarios) error {
	_, err := c.DeployServices(t.Context(), s.Missing(t))
	return expectProblem(err, problem.NotFound, "ServiceSet the provider cannot see", "ErrNotFound")
}

func checkStale(t *testing.T, c *providerclient.Client, s Scenarios) error {
	_, err := c.DeployServices(t.Context(), s.Stale(t))
	return expectProblem(err, problem.StaleGeneration, "request ahead of the generation the provider read", "ErrStaleGeneration")
}

func checkIdempotent(t *testing.T, c *providerclient.Client, s Scenarios) error {
	req := s.Deployable(t)
	first, err := c.DeployServices(t.Context(), req)
	if err != nil {
		return fmt.Errorf("first deploy failed: %w", err)
	}
	second, err := c.DeployServices(t.Context(), req)
	if err != nil {
		return fmt.Errorf("repeating the same deploy must succeed, kcm sends it on every reconcile: %w", err)
	}
	if first.Applied != second.Applied {
		return fmt.Errorf("repeated deploy changed applied from %v to %v", first.Applied, second.Applied)
	}
	if a, b := serviceKeys(first), serviceKeys(second); !slices.Equal(a, b) {
		return fmt.Errorf("repeated deploy changed the reported services from %v to %v", a, b)
	}
	return nil
}

func checkDelete(t *testing.T, c *providerclient.Client, s Scenarios) error {
	req := s.Deployable(t)
	if _, err := c.DeployServices(t.Context(), req); err != nil {
		return fmt.Errorf("deploy before delete failed: %w", err)
	}

	del := provider.DeleteServicesRequest{Name: req.Name, Namespace: req.Namespace}
	timeout := s.DeleteTimeout
	if timeout == 0 {
		timeout = defaultDeleteTimeout
	}
	deadline := time.Now().Add(timeout)
	for {
		res, err := c.DeleteServices(t.Context(), del)
		if err != nil {
			return fmt.Errorf("delete failed: %w", err)
		}
		if res.Done {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("delete did not report done within %s", timeout)
		}
		time.Sleep(deletePollInterval)
	}

	res, err := c.DeleteServices(t.Context(), del)
	if err != nil {
		return fmt.Errorf("deleting an already deleted ServiceSet must succeed, kcm retries until the finalizer is gone: %w", err)
	}
	if !res.Done {
		return errors.New("deleting an already deleted ServiceSet reported done=false")
	}
	return nil
}

func checkObservedGeneration(req provider.DeployServicesRequest, res provider.DeployServicesResult) error {
	if res.ObservedGeneration != req.Generation {
		return fmt.Errorf("observedGeneration = %d, want the generation the provider acted on (%d)",
			res.ObservedGeneration, req.Generation)
	}
	return nil
}

func expectProblem(err error, want problem.Type, situation, sentinel string) error {
	if err == nil {
		return fmt.Errorf("%s: deploy succeeded, want the provider to return %s", situation, sentinel)
	}
	pe, ok := errors.AsType[*providerclient.ProblemError](err)
	if !ok || pe.Type != want {
		return fmt.Errorf("%s: got %v, want a %s problem (return %s)", situation, err, want, sentinel)
	}
	return nil
}

func describeFailure(res provider.DeployServicesResult) string {
	reason, message := "<none>", "<none>"
	if res.Reason != nil {
		reason = string(*res.Reason)
	}
	if res.Message != nil {
		message = *res.Message
	}
	return "reason " + reason + ", message " + message
}

func serviceKeys(res provider.DeployServicesResult) []string {
	keys := make([]string, 0, len(res.Services))
	for _, svc := range res.Services {
		keys = append(keys, svc.Namespace+"/"+svc.Name)
	}
	slices.Sort(keys)
	return keys
}
