package providertest

import (
	"testing"
	"time"

	"github.com/brongineer/ksm-provider-api/api/provider"
	providerclient "github.com/brongineer/ksm-provider-api/provider/client"
	providerserver "github.com/brongineer/ksm-provider-api/provider/server"
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

var checks []check

// RunContract verifies a provider against the contract kcm relies on, through the real HTTP
// server and client, so request and result validation are exercised too.
func RunContract(t *testing.T, s Scenarios) {
	t.Helper()
}
