package providertest

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/brongineer/ksm-provider-api/api/provider"
	registryclient "github.com/brongineer/ksm-provider-api/registry/client"
)

func TestTestServerRoundTrip(t *testing.T) {
	f := &FakeProvider{}
	srv := NewTestServer(t, f)
	c := srv.Client(t)

	res, err := c.DeployServices(context.Background(), provider.DeployServicesRequest{Name: "a", Namespace: "b", Generation: 3})
	if err != nil {
		t.Fatalf("DeployServices: %v", err)
	}
	if res.ObservedGeneration != 3 || !res.Applied {
		t.Errorf("result = %+v, want applied at generation 3", res)
	}
	del, err := c.DeleteServices(context.Background(), provider.DeleteServicesRequest{Name: "a", Namespace: "b"})
	if err != nil || !del.Done {
		t.Errorf("DeleteServices = %+v, %v; want done", del, err)
	}
	if len(f.DeployCalls()) != 1 || len(f.DeleteCalls()) != 1 {
		t.Errorf("provider saw %d deploys and %d deletes, want 1 each", len(f.DeployCalls()), len(f.DeleteCalls()))
	}
}

func TestTestServerAuthenticatesCallers(t *testing.T) {
	srv := NewTestServer(t, &FakeProvider{})
	tests := []struct {
		name  string
		token string
		want  int
	}{
		{"caller token", CallerToken, http.StatusOK},
		{"other token", "nope", http.StatusUnauthorized},
		{"no token", "", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/services:deploy",
				strings.NewReader(`{"name":"a","namespace":"b","generation":1}`))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			resp, err := srv.HTTPClient.Do(req)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tt.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tt.want)
			}
		})
	}
}

func TestFakeRegistryAcceptsRealHeartbeater(t *testing.T) {
	reg := NewFakeRegistry(t, "sveltos")
	hb, err := registryclient.NewHeartbeater(registryclient.Config{
		RegistryURL: reg.URL,
		TokenFile:   reg.TokenFile,
		HTTPClient:  reg.HTTPClient,
		Name:        "sveltos",
		Endpoint:    "https://ksm-provider-sveltos.projectsveltos.svc:9443",
		APIVersions: []string{"v1"},
		Interval:    time.Second,
	})
	if err != nil {
		t.Fatalf("NewHeartbeater: %v", err)
	}
	ctx := t.Context()
	go func() { _ = hb.Start(ctx) }()

	select {
	case <-hb.Registered():
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeater did not register")
	}
	entry, ok := reg.Registry.Lookup("sveltos")
	if !ok || entry.Endpoint != "https://ksm-provider-sveltos.projectsveltos.svc:9443" {
		t.Errorf("Lookup = %+v, %v; want the registered endpoint", entry, ok)
	}
}

func TestFakeRegistryRejectsUnlistedNames(t *testing.T) {
	reg := NewFakeRegistry(t, "sveltos")
	req, err := http.NewRequest(http.MethodPost, reg.URL+"/v1/providers/register", strings.NewReader(
		`{"name":"other","endpoint":"https://a.svc","state":"Active","ttlSeconds":3,"apiVersions":["v1"]}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+ProviderToken)
	resp, err := reg.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if _, ok := reg.Registry.Lookup("other"); ok {
		t.Error("unlisted name was registered")
	}
}
