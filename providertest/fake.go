package providertest

import (
	"context"
	"slices"
	"sync"

	"github.com/brongineer/ksm-provider-api/api/provider"
)

// FakeProvider is a scripted provider for kcm-side tests. Without funcs it reports every
// ServiceSet as applied at the requested generation with no services, and every delete as done.
type FakeProvider struct {
	DeployFunc  func(ctx context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error)
	DeleteFunc  func(ctx context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error)
	deployCalls []provider.DeployServicesRequest
	deleteCalls []provider.DeleteServicesRequest
	mu          sync.Mutex
}

func (f *FakeProvider) DeployServices(ctx context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error) {
	f.mu.Lock()
	f.deployCalls = append(f.deployCalls, req)
	fn := f.DeployFunc
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, req)
	}
	return provider.DeployServicesResult{
		ObservedGeneration: req.Generation,
		Applied:            true,
		Collected:          true,
		Services:           []provider.ServiceState{},
	}, nil
}

func (f *FakeProvider) DeleteServices(ctx context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error) {
	f.mu.Lock()
	f.deleteCalls = append(f.deleteCalls, req)
	fn := f.DeleteFunc
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, req)
	}
	return provider.DeleteServicesResult{Done: true}, nil
}

// DeployCalls returns the deploy requests received so far, in order.
func (f *FakeProvider) DeployCalls() []provider.DeployServicesRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deployCalls)
}

// DeleteCalls returns the delete requests received so far, in order.
func (f *FakeProvider) DeleteCalls() []provider.DeleteServicesRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.deleteCalls)
}
