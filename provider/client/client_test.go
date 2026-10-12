package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brongineer/ksm-provider-api/api/provider"
	"github.com/brongineer/ksm-provider-api/problem"
)

type captured struct {
	method        string
	path          string
	contentType   string
	authorization string
	body          map[string]any
}

type fakeProvider struct {
	server *httptest.Server
	last   chan captured
	reply  func(w http.ResponseWriter)
}

func newFakeProvider(t *testing.T, reply func(w http.ResponseWriter)) *fakeProvider {
	t.Helper()
	f := &fakeProvider{last: make(chan captured, 16), reply: reply}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		f.last <- captured{
			method:        r.Method,
			path:          r.URL.Path,
			contentType:   r.Header.Get("Content-Type"),
			authorization: r.Header.Get("Authorization"),
			body:          body,
		}
		f.reply(w)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func okJSON(body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}
}

func writeToken(t *testing.T, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
}

func newTestClient(t *testing.T, endpoint string) (*Client, string) {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "kcm-token-1\n")
	c, err := New(Config{Endpoint: endpoint, TokenFile: tokenFile, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, tokenFile
}

const deployedJSON = `{"observedGeneration":7,"applied":true,"collected":true,"services":[` +
	`{"name":"ingress-nginx","namespace":"ingress","template":"ingress-nginx-4-11-0","type":"Helm",` +
	`"state":"Deployed","deployedHash":"abc123","failureMessage":"none"}]}`

var deployReq = provider.DeployServicesRequest{Name: "cd-1-abcd", Namespace: "team-a", Generation: 7}

func TestDeployServices(t *testing.T) {
	f := newFakeProvider(t, okJSON(deployedJSON))
	c, _ := newTestClient(t, f.server.URL)

	got, err := c.DeployServices(context.Background(), deployReq)
	if err != nil {
		t.Fatalf("DeployServices: %v", err)
	}

	req := <-f.last
	if req.method != http.MethodPost || req.path != "/v1/services:deploy" {
		t.Errorf("request = %s %s, want POST /v1/services:deploy", req.method, req.path)
	}
	if req.authorization != "Bearer kcm-token-1" {
		t.Errorf("Authorization = %q, want %q", req.authorization, "Bearer kcm-token-1")
	}
	if req.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", req.contentType)
	}
	if req.body["name"] != "cd-1-abcd" || req.body["namespace"] != "team-a" || req.body["generation"] != float64(7) {
		t.Errorf("body = %v, want the request fields", req.body)
	}
	if got.ObservedGeneration != 7 || len(got.Services) != 1 || *got.Services[0].DeployedHash != "abc123" ||
		*got.Services[0].FailureMessage != "none" {
		t.Errorf("result = %+v, want the decoded response", got)
	}
}

func TestDeleteServices(t *testing.T) {
	f := newFakeProvider(t, okJSON(`{"done":true}`))
	c, _ := newTestClient(t, f.server.URL)

	got, err := c.DeleteServices(context.Background(), provider.DeleteServicesRequest{Name: "cd-1-abcd", Namespace: "team-a"})
	if err != nil {
		t.Fatalf("DeleteServices: %v", err)
	}

	req := <-f.last
	if req.path != "/v1/services:delete" {
		t.Errorf("path = %q, want /v1/services:delete", req.path)
	}
	if !got.Done {
		t.Error("Done = false, want true")
	}
}

func TestTokenIsRereadOnEveryCall(t *testing.T) {
	f := newFakeProvider(t, okJSON(deployedJSON))
	c, tokenFile := newTestClient(t, f.server.URL)

	if _, err := c.DeployServices(context.Background(), deployReq); err != nil {
		t.Fatalf("first call: %v", err)
	}
	<-f.last
	writeToken(t, tokenFile, "kcm-token-2")
	if _, err := c.DeployServices(context.Background(), deployReq); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if got := (<-f.last).authorization; got != "Bearer kcm-token-2" {
		t.Errorf("Authorization = %q, want the rotated token", got)
	}
}

func TestProblemResponsesBecomeProblemErrors(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		typ      problem.Type
		viaProxy bool
	}{
		{"bad request", http.StatusBadRequest, problem.InvalidRequest, false},
		{"unauthenticated", http.StatusUnauthorized, problem.Unauthenticated, false},
		{"forbidden", http.StatusForbidden, problem.Forbidden, false},
		{"not found", http.StatusNotFound, problem.NotFound, false},
		{"stale generation", http.StatusConflict, problem.StaleGeneration, false},
		{"internal", http.StatusInternalServerError, problem.Internal, false},
		{"proxy error page", http.StatusBadGateway, problem.Internal, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeProvider(t, func(w http.ResponseWriter) {
				if tt.viaProxy {
					w.Header().Set("Content-Type", "text/plain")
					w.WriteHeader(tt.status)
					_, _ = io.WriteString(w, "upstream connect error")
					return
				}
				problem.Write(w, problem.New(tt.typ, "detail"))
			})
			c, _ := newTestClient(t, f.server.URL)

			_, err := c.DeployServices(context.Background(), deployReq)

			var pe *ProblemError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want *ProblemError", err, err)
			}
			if pe.Type != tt.typ || pe.Status != tt.status {
				t.Errorf("problem = %s/%d, want %s/%d", pe.Type, pe.Status, tt.typ, tt.status)
			}
		})
	}
}

func TestInvalidResultsAreRejected(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not JSON", `{"observedGeneration":`},
		{"unknown service state", `{"observedGeneration":7,"applied":true,"collected":true,"services":[` +
			`{"name":"a","namespace":"b","template":"c","type":"Helm","state":"Exploded"}]}`},
		{"missing required field", `{"observedGeneration":7,"applied":true,"services":[]}`},
		{"services is null", `{"observedGeneration":7,"applied":true,"collected":true,"services":null}`},
		{"not applied without reason", `{"observedGeneration":7,"applied":false,"collected":false,"services":[],"message":"x"}`},
		{"not applied without message", `{"observedGeneration":7,"applied":false,"collected":false,"services":[],"reason":"ProviderError"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeProvider(t, okJSON(tt.body))
			c, _ := newTestClient(t, f.server.URL)

			_, err := c.DeployServices(context.Background(), deployReq)

			if !errors.Is(err, ErrInvalidResult) {
				t.Errorf("err = %v, want ErrInvalidResult", err)
			}
		})
	}
}

func TestInvalidDeleteResultIsRejected(t *testing.T) {
	f := newFakeProvider(t, okJSON(`{"done":"yes"}`))
	c, _ := newTestClient(t, f.server.URL)

	_, err := c.DeleteServices(context.Background(), provider.DeleteServicesRequest{Name: "a", Namespace: "b"})

	if !errors.Is(err, ErrInvalidResult) {
		t.Errorf("err = %v, want ErrInvalidResult", err)
	}
}

func TestTimeout(t *testing.T) {
	release := make(chan struct{})
	f := newFakeProvider(t, func(w http.ResponseWriter) {
		<-release
		w.WriteHeader(http.StatusOK)
	})
	// Registered after the server so it runs before server.Close, which waits for handlers.
	t.Cleanup(func() { close(release) })
	tokenFile := filepath.Join(t.TempDir(), "token")
	writeToken(t, tokenFile, "kcm-token")
	c, err := New(Config{Endpoint: f.server.URL, TokenFile: tokenFile, Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	_, err = c.DeployServices(context.Background(), deployReq)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if _, ok := errors.AsType[*ProblemError](err); ok {
		t.Errorf("err is a ProblemError, want a transport error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("call took %s, want it bounded by the timeout", elapsed)
	}
}

func TestCallerCancellation(t *testing.T) {
	release := make(chan struct{})
	f := newFakeProvider(t, func(w http.ResponseWriter) {
		<-release
		w.WriteHeader(http.StatusOK)
	})
	t.Cleanup(func() { close(release) })
	c, _ := newTestClient(t, f.server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-f.last
		cancel()
	}()

	_, err := c.DeployServices(ctx, deployReq)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestMissingTokenFileFailsTheCall(t *testing.T) {
	f := newFakeProvider(t, okJSON(deployedJSON))
	c, tokenFile := newTestClient(t, f.server.URL)
	if err := os.Remove(tokenFile); err != nil {
		t.Fatalf("remove token: %v", err)
	}

	if _, err := c.DeployServices(context.Background(), deployReq); err == nil {
		t.Error("DeployServices succeeded without a token, want an error")
	}
	select {
	case <-f.last:
		t.Error("request sent without a token")
	default:
	}
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"missing endpoint", Config{TokenFile: "/token", Timeout: time.Second}},
		{"relative endpoint", Config{Endpoint: "/v1", TokenFile: "/token", Timeout: time.Second}},
		{"missing token file", Config{Endpoint: "https://a.svc", Timeout: time.Second}},
		{"zero timeout", Config{Endpoint: "https://a.svc", TokenFile: "/token"}},
		{"CA bundle without certificates", Config{Endpoint: "https://a.svc", TokenFile: "/token", Timeout: time.Second, CABundle: []byte("x")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Error("New succeeded, want an error")
			}
		})
	}
}
