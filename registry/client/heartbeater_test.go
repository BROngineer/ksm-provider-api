package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/brongineer/ksm-provider-api/problem"
)

const waitTimeout = 5 * time.Second

type receivedBeat struct {
	body  map[string]any
	token string
}

type fakeRegistry struct {
	server *httptest.Server
	beats  chan receivedBeat
	status int
	mu     sync.Mutex
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{beats: make(chan receivedBeat, 64), status: http.StatusNoContent}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/providers/register" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("registry received invalid JSON: %v", err)
		}
		f.beats <- receivedBeat{body: body, token: r.Header.Get("Authorization")}

		f.mu.Lock()
		status := f.status
		f.mu.Unlock()
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		problem.Write(w, problem.Problem{Type: problem.NotLeader, Title: "Not leader", Status: status})
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeRegistry) setStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeRegistry) next(t *testing.T) receivedBeat {
	t.Helper()
	select {
	case b := <-f.beats:
		return b
	case <-time.After(waitTimeout):
		t.Fatal("no heartbeat received")
		return receivedBeat{}
	}
}

func (f *fakeRegistry) assertNoBeat(t *testing.T) {
	t.Helper()
	select {
	case b := <-f.beats:
		t.Fatalf("unexpected heartbeat: %v", b.body)
	case <-time.After(100 * time.Millisecond):
	}
}

func writeToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
}

func baseConfig(t *testing.T, registryURL string) Config {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "token-1\n")
	return Config{
		RegistryURL:      registryURL,
		TokenFile:        tokenFile,
		Name:             "sveltos",
		Endpoint:         "https://ksm-provider-sveltos.projectsveltos.svc:9443",
		EndpointCABundle: []byte("ca-pem"),
		APIVersions:      []string{"v1"},
		Interval:         2 * time.Second,
	}
}

type manualTicker struct {
	c       chan time.Time
	stopped chan struct{}
}

func newManualTicker() *manualTicker {
	return &manualTicker{c: make(chan time.Time), stopped: make(chan struct{})}
}

func (m *manualTicker) factory(time.Duration) (<-chan time.Time, func()) {
	return m.c, func() { close(m.stopped) }
}

func (m *manualTicker) tick(t *testing.T) {
	t.Helper()
	select {
	case m.c <- time.Now():
	case <-time.After(waitTimeout):
		t.Fatal("heartbeater is not waiting for a tick")
	}
}

type running struct {
	cancel context.CancelFunc
	errc   chan error
}

func start(t *testing.T, h *Heartbeater) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{cancel: cancel, errc: make(chan error, 1)}
	go func() { r.errc <- h.Start(ctx) }()
	t.Cleanup(cancel)
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.errc:
		if err != nil {
			t.Errorf("Start returned %v, want nil", err)
		}
	case <-time.After(waitTimeout):
		t.Fatal("Start did not return after cancel")
	}
}

func newTestHeartbeater(t *testing.T, cfg Config) (*Heartbeater, *manualTicker) {
	t.Helper()
	h, err := NewHeartbeater(cfg)
	if err != nil {
		t.Fatalf("NewHeartbeater: %v", err)
	}
	ticker := newManualTicker()
	h.newTicker = ticker.factory
	return h, ticker
}

func TestNewHeartbeaterValidatesConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"interval below minimum", func(c *Config) { c.Interval = 999 * time.Millisecond }},
		{"zero interval", func(c *Config) { c.Interval = 0 }},
		{"missing registry URL", func(c *Config) { c.RegistryURL = "" }},
		{"relative registry URL", func(c *Config) { c.RegistryURL = "/registry" }},
		{"missing token file", func(c *Config) { c.TokenFile = "" }},
		{"missing name", func(c *Config) { c.Name = "" }},
		{"missing endpoint", func(c *Config) { c.Endpoint = "" }},
		{"no api versions", func(c *Config) { c.APIVersions = nil }},
		{"registry CA bundle without certificates", func(c *Config) { c.RegistryCABundle = []byte("not a pem") }},
		{"negative provider timeout", func(c *Config) { c.ProviderTimeout = -time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig(t, "https://kcm-registry.kcm-system.svc:8443")
			tt.mutate(&cfg)
			if _, err := NewHeartbeater(cfg); err == nil {
				t.Error("NewHeartbeater succeeded, want an error")
			}
		})
	}
}

func TestFirstHeartbeatIsSentImmediately(t *testing.T) {
	reg := newFakeRegistry(t)
	cfg := baseConfig(t, reg.server.URL)
	cfg.ProviderTimeout = 45 * time.Second
	h, _ := newTestHeartbeater(t, cfg)

	run := start(t, h)
	beat := reg.next(t)
	run.stop(t)

	if beat.token != "Bearer token-1" {
		t.Errorf("Authorization = %q, want %q", beat.token, "Bearer token-1")
	}
	want := map[string]any{
		"name":           "sveltos",
		"endpoint":       "https://ksm-provider-sveltos.projectsveltos.svc:9443",
		"state":          "Active",
		"caBundle":       "Y2EtcGVt",
		"apiVersions":    []any{"v1"},
		"ttlSeconds":     float64(6),
		"timeoutSeconds": float64(45),
	}
	for k, v := range want {
		if got := mustJSON(t, beat.body[k]); got != mustJSON(t, v) {
			t.Errorf("body[%q] = %s, want %s", k, got, mustJSON(t, v))
		}
	}
}

func TestOptionalFieldsAreOmitted(t *testing.T) {
	reg := newFakeRegistry(t)
	cfg := baseConfig(t, reg.server.URL)
	cfg.EndpointCABundle = nil
	h, _ := newTestHeartbeater(t, cfg)

	run := start(t, h)
	beat := reg.next(t)
	run.stop(t)

	for _, k := range []string{"caBundle", "timeoutSeconds"} {
		if _, ok := beat.body[k]; ok {
			t.Errorf("body contains %q, want it omitted", k)
		}
	}
}

func TestTTLIsRoundedUp(t *testing.T) {
	reg := newFakeRegistry(t)
	cfg := baseConfig(t, reg.server.URL)
	cfg.Interval = 1500 * time.Millisecond
	h, _ := newTestHeartbeater(t, cfg)

	run := start(t, h)
	beat := reg.next(t)
	run.stop(t)

	if got := beat.body["ttlSeconds"]; got != float64(5) {
		t.Errorf("ttlSeconds = %v, want 5 (3 x 1.5s rounded up)", got)
	}
}

func TestHeartbeatsOnEveryTickAndRereadsToken(t *testing.T) {
	reg := newFakeRegistry(t)
	cfg := baseConfig(t, reg.server.URL)
	h, ticker := newTestHeartbeater(t, cfg)

	run := start(t, h)
	reg.next(t)

	writeToken(t, cfg.TokenFile, "token-2")
	ticker.tick(t)
	beat := reg.next(t)
	run.stop(t)

	if beat.token != "Bearer token-2" {
		t.Errorf("Authorization = %q, want rotated token", beat.token)
	}
}

func TestRegisteredClosesOnlyAfterAcceptedHeartbeat(t *testing.T) {
	reg := newFakeRegistry(t)
	reg.setStatus(http.StatusServiceUnavailable)
	h, ticker := newTestHeartbeater(t, baseConfig(t, reg.server.URL))

	run := start(t, h)
	reg.next(t)
	select {
	case <-h.Registered():
		t.Fatal("Registered closed after a rejected heartbeat")
	case <-time.After(100 * time.Millisecond):
	}

	reg.setStatus(http.StatusNoContent)
	ticker.tick(t)
	reg.next(t)
	select {
	case <-h.Registered():
	case <-time.After(waitTimeout):
		t.Fatal("Registered not closed after an accepted heartbeat")
	}

	ticker.tick(t)
	reg.next(t)
	run.stop(t)
}

func TestFailedHeartbeatIsRetriedOnNextTick(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"not leader", http.StatusServiceUnavailable},
		{"internal error", http.StatusInternalServerError},
		{"forbidden", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newFakeRegistry(t)
			reg.setStatus(tt.status)
			h, ticker := newTestHeartbeater(t, baseConfig(t, reg.server.URL))

			run := start(t, h)
			reg.next(t)
			ticker.tick(t)
			reg.next(t)
			run.stop(t)
		})
	}
}

func TestUnreachableRegistryIsRetried(t *testing.T) {
	reg := newFakeRegistry(t)
	url := reg.server.URL
	reg.server.Close()
	h, ticker := newTestHeartbeater(t, baseConfig(t, url))

	run := start(t, h)
	ticker.tick(t)
	ticker.tick(t)
	run.stop(t)
}

func TestShutdownHeartbeat(t *testing.T) {
	tests := []struct {
		name          string
		check         func(context.Context) (bool, error)
		wantSuspended bool
	}{
		{"scaled to zero", func(context.Context) (bool, error) { return true, nil }, true},
		{"not scaled to zero", func(context.Context) (bool, error) { return false, nil }, false},
		{"check fails", func(context.Context) (bool, error) { return false, errors.New("api down") }, false},
		{"no check configured", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := newFakeRegistry(t)
			cfg := baseConfig(t, reg.server.URL)
			cfg.SuspendCheck = tt.check
			h, _ := newTestHeartbeater(t, cfg)

			run := start(t, h)
			reg.next(t)
			run.stop(t)

			if !tt.wantSuspended {
				reg.assertNoBeat(t)
				return
			}
			beat := reg.next(t)
			if beat.body["state"] != "Suspended" {
				t.Errorf("shutdown heartbeat state = %v, want Suspended", beat.body["state"])
			}
			if beat.body["ttlSeconds"] != float64(6) {
				t.Errorf("shutdown heartbeat ttlSeconds = %v, want 6", beat.body["ttlSeconds"])
			}
		})
	}
}

func TestSuspendCheckGetsLiveContext(t *testing.T) {
	reg := newFakeRegistry(t)
	cfg := baseConfig(t, reg.server.URL)
	var checkErr error
	cfg.SuspendCheck = func(ctx context.Context) (bool, error) {
		checkErr = ctx.Err()
		return true, nil
	}
	h, _ := newTestHeartbeater(t, cfg)

	run := start(t, h)
	reg.next(t)
	run.stop(t)
	reg.next(t)

	if checkErr != nil {
		t.Errorf("SuspendCheck context already done: %v", checkErr)
	}
}

func TestTickerIsStoppedOnShutdown(t *testing.T) {
	reg := newFakeRegistry(t)
	h, ticker := newTestHeartbeater(t, baseConfig(t, reg.server.URL))

	run := start(t, h)
	reg.next(t)
	run.stop(t)

	select {
	case <-ticker.stopped:
	case <-time.After(waitTimeout):
		t.Error("ticker not stopped after Start returned")
	}
}

func TestHeartbeaterNeedsLeaderElection(t *testing.T) {
	h, err := NewHeartbeater(baseConfig(t, "https://kcm-registry.kcm-system.svc:8443"))
	if err != nil {
		t.Fatalf("NewHeartbeater: %v", err)
	}
	if !h.NeedLeaderElection() {
		t.Error("NeedLeaderElection = false, want true")
	}
}

func TestDeploymentScaledToZero(t *testing.T) {
	key := types.NamespacedName{Namespace: "projectsveltos", Name: "ksm-provider-sveltos"}
	deployment := func(replicas *int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			Namespace: key.Namespace, Name: key.Name,
			Spec: appsv1.DeploymentSpec{Replicas: replicas},
		}
	}
	tests := []struct {
		name    string
		objects []*appsv1.Deployment
		want    bool
		wantErr bool
	}{
		{"scaled to zero", []*appsv1.Deployment{deployment(new(int32(0)))}, true, false},
		{"running replicas", []*appsv1.Deployment{deployment(new(int32(2)))}, false, false},
		{"replicas unset defaults to one", []*appsv1.Deployment{deployment(nil)}, false, false},
		{"deployment missing", nil, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewClientset()
			for _, d := range tt.objects {
				if _, err := cs.AppsV1().Deployments(d.Namespace).Create(context.Background(), d, metav1.CreateOptions{}); err != nil {
					t.Fatalf("create deployment: %v", err)
				}
			}

			got, err := DeploymentScaledToZero(cs, key)(context.Background())

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("scaled to zero = %v, want %v", got, tt.want)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
