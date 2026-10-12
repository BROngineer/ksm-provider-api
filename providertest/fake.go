package providertest

import (
	"context"

	"github.com/brongineer/ksm-provider-api/api/provider"
)

// FakeProvider is a scripted provider for kcm-side tests. Without funcs it reports every
// ServiceSet as applied at the requested generation with no services, and every delete as done.
type FakeProvider struct {
	DeployFunc func(ctx context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error)
	DeleteFunc func(ctx context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error)
}

func (f *FakeProvider) DeployServices(ctx context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error) {
	return provider.DeployServicesResult{}, nil
}

func (f *FakeProvider) DeleteServices(ctx context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error) {
	return provider.DeleteServicesResult{}, nil
}

// DeployCalls returns the deploy requests received so far, in order.
func (f *FakeProvider) DeployCalls() []provider.DeployServicesRequest {
	return nil
}

// DeleteCalls returns the delete requests received so far, in order.
func (f *FakeProvider) DeleteCalls() []provider.DeleteServicesRequest {
	return nil
}
