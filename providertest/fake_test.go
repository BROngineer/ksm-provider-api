package providertest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/brongineer/ksm-provider-api/api/provider"
)

func TestFakeProviderDefaults(t *testing.T) {
	f := &FakeProvider{}
	req := provider.DeployServicesRequest{Name: "a", Namespace: "b", Generation: 4}

	res, err := f.DeployServices(context.Background(), req)
	if err != nil {
		t.Fatalf("DeployServices: %v", err)
	}
	if !res.Applied || !res.Collected || res.ObservedGeneration != 4 || res.Services == nil || len(res.Services) != 0 {
		t.Errorf("result = %+v, want applied and collected at generation 4 with no services", res)
	}

	del, err := f.DeleteServices(context.Background(), provider.DeleteServicesRequest{Name: "a", Namespace: "b"})
	if err != nil || !del.Done {
		t.Errorf("DeleteServices = %+v, %v; want done", del, err)
	}
}

func TestFakeProviderUsesScriptedFuncs(t *testing.T) {
	boom := errors.New("boom")
	f := &FakeProvider{
		DeployFunc: func(context.Context, provider.DeployServicesRequest) (provider.DeployServicesResult, error) {
			return provider.DeployServicesResult{}, boom
		},
		DeleteFunc: func(context.Context, provider.DeleteServicesRequest) (provider.DeleteServicesResult, error) {
			return provider.DeleteServicesResult{Done: false}, nil
		},
	}

	if _, err := f.DeployServices(context.Background(), provider.DeployServicesRequest{}); !errors.Is(err, boom) {
		t.Errorf("DeployServices err = %v, want the scripted error", err)
	}
	if res, _ := f.DeleteServices(context.Background(), provider.DeleteServicesRequest{}); res.Done {
		t.Error("DeleteServices done = true, want the scripted false")
	}
}

func TestFakeProviderRecordsCalls(t *testing.T) {
	f := &FakeProvider{}
	first := provider.DeployServicesRequest{Name: "a", Namespace: "ns", Generation: 1}
	second := provider.DeployServicesRequest{Name: "b", Namespace: "ns", Generation: 2}
	del := provider.DeleteServicesRequest{Name: "a", Namespace: "ns"}

	_, _ = f.DeployServices(context.Background(), first)
	_, _ = f.DeployServices(context.Background(), second)
	_, _ = f.DeleteServices(context.Background(), del)

	if got := f.DeployCalls(); len(got) != 2 || got[0] != first || got[1] != second {
		t.Errorf("DeployCalls = %+v, want [%+v %+v]", got, first, second)
	}
	if got := f.DeleteCalls(); len(got) != 1 || got[0] != del {
		t.Errorf("DeleteCalls = %+v, want [%+v]", got, del)
	}

	calls := f.DeployCalls()
	calls[0].Name = "mutated"
	if f.DeployCalls()[0].Name != "a" {
		t.Error("DeployCalls exposes internal state")
	}
}

func TestFakeProviderIsSafeForConcurrentUse(t *testing.T) {
	f := &FakeProvider{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				_, _ = f.DeployServices(context.Background(), provider.DeployServicesRequest{Name: "a", Namespace: "b"})
				_, _ = f.DeleteServices(context.Background(), provider.DeleteServicesRequest{Name: "a", Namespace: "b"})
				_ = f.DeployCalls()
			}
		})
	}
	wg.Wait()

	if got := len(f.DeployCalls()); got != 400 {
		t.Errorf("recorded %d deploy calls, want 400", got)
	}
}
