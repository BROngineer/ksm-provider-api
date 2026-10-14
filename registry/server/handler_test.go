package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brongineer/ksm-provider-api/auth"
	"github.com/brongineer/ksm-provider-api/problem"
)

const (
	registerPath = "/v1/providers/register"
	sveltosUser  = "system:serviceaccount:projectsveltos:ksm-provider-sveltos"
	otherUser    = "system:serviceaccount:other:ksm-provider-other"
)

type tokenAuthenticator struct {
	users map[string]string
	calls int
}

func (a *tokenAuthenticator) Authenticate(_ context.Context, token string) (string, error) {
	a.calls++
	if user, ok := a.users[token]; ok {
		return user, nil
	}
	return "", auth.ErrUnauthenticated
}

type handlerFixture struct {
	handler  http.Handler
	registry *Registry
	authn    *tokenAuthenticator
	clock    *fakeClock
	leader   bool
}

func newHandlerFixture(t *testing.T) *handlerFixture {
	t.Helper()
	f := &handlerFixture{
		clock:  newFakeClock(),
		leader: true,
		authn: &tokenAuthenticator{users: map[string]string{
			"sveltos-token": sveltosUser,
			"other-token":   otherUser,
		}},
	}
	f.registry = NewRegistry(WithClock(f.clock.now))
	h, err := NewHandler(HandlerConfig{
		Registry:      f.registry,
		Authenticator: f.authn,
		Allowed:       map[string]string{"sveltos": sveltosUser, "other": otherUser},
		IsLeader:      func() bool { return f.leader },
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	f.handler = h
	return f
}

func (f *handlerFixture) do(t *testing.T, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func validBody(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	body := map[string]any{
		"name":        "sveltos",
		"endpoint":    "https://ksm-provider-sveltos.projectsveltos.svc:9443",
		"state":       "Active",
		"caBundle":    base64.StdEncoding.EncodeToString([]byte("ca-pem")),
		"ttlSeconds":  30,
		"apiVersions": []string{"v1"},
	}
	if mutate != nil {
		mutate(body)
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func decodeProblem(t *testing.T, rec *httptest.ResponseRecorder) problem.Problem {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
		t.Fatalf("Content-Type = %q, want %q (body %s)", got, problem.ContentType, rec.Body.String())
	}
	var p problem.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a problem: %v", err)
	}
	return p
}

func TestHandlerRegistersProvider(t *testing.T) {
	f := newHandlerFixture(t)

	rec := f.do(t, "sveltos-token", validBody(t, nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	got, ok := f.registry.Lookup("sveltos")
	if !ok {
		t.Fatal("provider not registered")
	}
	want := Registration{
		Name:        "sveltos",
		Endpoint:    "https://ksm-provider-sveltos.projectsveltos.svc:9443",
		State:       StateActive,
		CABundle:    []byte("ca-pem"),
		APIVersions: []string{"v1"},
		TTL:         30 * time.Second,
		Timeout:     defaultTimeout * time.Second,
	}
	if got.Name != want.Name || got.Endpoint != want.Endpoint || got.State != want.State ||
		string(got.CABundle) != string(want.CABundle) || !slices.Equal(got.APIVersions, want.APIVersions) ||
		got.TTL != want.TTL || got.Timeout != want.Timeout {
		t.Errorf("registration = %+v, want %+v", got.Registration, want)
	}
}

func TestHandlerMapsOptionalFields(t *testing.T) {
	f := newHandlerFixture(t)

	rec := f.do(t, "sveltos-token", validBody(t, func(b map[string]any) {
		delete(b, "caBundle")
		b["timeoutSeconds"] = 45
		b["state"] = "Suspended"
	}))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	got, _ := f.registry.Lookup("sveltos")
	if got.Timeout != 45*time.Second || got.CABundle != nil || got.State != StateSuspended {
		t.Errorf("registration = %+v, want timeout 45s, no CA bundle, suspended", got.Registration)
	}
}

func TestHandlerRejectsNonLeaderBeforeAuthentication(t *testing.T) {
	f := newHandlerFixture(t)
	f.leader = false

	rec := f.do(t, "", validBody(t, nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if p := decodeProblem(t, rec); p.Type != problem.NotLeader {
		t.Errorf("problem type = %q, want %q", p.Type, problem.NotLeader)
	}
	if f.authn.calls != 0 {
		t.Errorf("authenticator called %d times on a non-leader", f.authn.calls)
	}
}

func TestHandlerAuthentication(t *testing.T) {
	tests := []struct {
		name  string
		token string
	}{
		{"missing token", ""},
		{"rejected token", "bogus"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHandlerFixture(t)

			rec := f.do(t, tt.token, validBody(t, nil))

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if p := decodeProblem(t, rec); p.Type != problem.Unauthenticated {
				t.Errorf("problem type = %q, want %q", p.Type, problem.Unauthenticated)
			}
			if _, ok := f.registry.Lookup("sveltos"); ok {
				t.Error("provider registered without authentication")
			}
		})
	}
}

func TestHandlerAuthorization(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		provider string
	}{
		{"service account registers another provider's name", "other-token", "sveltos"},
		{"name without an allow-list entry", "sveltos-token", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHandlerFixture(t)

			rec := f.do(t, tt.token, validBody(t, func(b map[string]any) { b["name"] = tt.provider }))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
			}
			if p := decodeProblem(t, rec); p.Type != problem.Forbidden {
				t.Errorf("problem type = %q, want %q", p.Type, problem.Forbidden)
			}
			if _, ok := f.registry.Lookup(tt.provider); ok {
				t.Error("provider registered by a forbidden caller")
			}
		})
	}
}

func TestHandlerValidation(t *testing.T) {
	tests := []struct {
		name      string
		body      func(t *testing.T) string
		wantField string
	}{
		{"malformed JSON", func(*testing.T) string { return `{"name":` }, ""},
		{"missing ttlSeconds", func(t *testing.T) string {
			return validBody(t, func(b map[string]any) { delete(b, "ttlSeconds") })
		}, "ttlSeconds"},
		{"ttlSeconds below minimum", func(t *testing.T) string {
			return validBody(t, func(b map[string]any) { b["ttlSeconds"] = 0 })
		}, "ttlSeconds"},
		{"ttlSeconds not an integer", func(t *testing.T) string {
			return validBody(t, func(b map[string]any) { b["ttlSeconds"] = 1.5 })
		}, "ttlSeconds"},
		{"name violates pattern", func(t *testing.T) string {
			return validBody(t, func(b map[string]any) { b["name"] = "Sveltos_" })
		}, "name"},
		{"unknown state", func(t *testing.T) string {
			return validBody(t, func(b map[string]any) { b["state"] = "Paused" })
		}, "state"},
		{"empty apiVersions", func(t *testing.T) string {
			return validBody(t, func(b map[string]any) { b["apiVersions"] = []string{} })
		}, "apiVersions"},
		{"relative endpoint", func(t *testing.T) string {
			return validBody(t, func(b map[string]any) { b["endpoint"] = "/just/a/path" })
		}, "endpoint"},
		{"endpoint with unsupported scheme", func(t *testing.T) string {
			return validBody(t, func(b map[string]any) { b["endpoint"] = "ftp://ksm.svc" })
		}, "endpoint"},
		{"caBundle not base64", func(t *testing.T) string {
			return validBody(t, func(b map[string]any) { b["caBundle"] = "%%%not-base64%%%" })
		}, ""},
		{"body too large", func(t *testing.T) string {
			return validBody(t, func(b map[string]any) { b["caBundle"] = strings.Repeat("A", maxRequestBytes) })
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newHandlerFixture(t)

			rec := f.do(t, "sveltos-token", tt.body(t))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			p := decodeProblem(t, rec)
			if p.Type != problem.InvalidRequest {
				t.Errorf("problem type = %q, want %q", p.Type, problem.InvalidRequest)
			}
			if tt.wantField != "" && !slices.ContainsFunc(p.Errors, func(e problem.FieldError) bool { return e.Field == tt.wantField }) {
				t.Errorf("errors = %+v, want an entry for field %q", p.Errors, tt.wantField)
			}
			if _, ok := f.registry.Lookup("sveltos"); ok {
				t.Error("provider registered from an invalid request")
			}
		})
	}
}

func TestHandlerReportsAllFieldErrors(t *testing.T) {
	f := newHandlerFixture(t)

	rec := f.do(t, "sveltos-token", validBody(t, func(b map[string]any) {
		b["ttlSeconds"] = 0
		b["state"] = "Paused"
	}))

	p := decodeProblem(t, rec)
	for _, field := range []string{"ttlSeconds", "state"} {
		if !slices.ContainsFunc(p.Errors, func(e problem.FieldError) bool { return e.Field == field }) {
			t.Errorf("errors = %+v, want an entry for field %q", p.Errors, field)
		}
	}
}

func TestHandlerDoesNotEchoRejectedValues(t *testing.T) {
	f := newHandlerFixture(t)
	const secretish = "Sveltos_SECRET_VALUE"

	rec := f.do(t, "sveltos-token", validBody(t, func(b map[string]any) { b["name"] = secretish }))

	if strings.Contains(rec.Body.String(), secretish) {
		t.Errorf("response echoes the rejected value: %s", rec.Body.String())
	}
}

func TestNewHandlerRequiresDependencies(t *testing.T) {
	tests := []struct {
		name string
		cfg  HandlerConfig
	}{
		{"no registry", HandlerConfig{Authenticator: &tokenAuthenticator{}}},
		{"no authenticator", HandlerConfig{Registry: NewRegistry()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewHandler(tt.cfg); err == nil {
				t.Error("NewHandler succeeded, want an error")
			}
		})
	}
}

func TestHandlerAuthorizeFunc(t *testing.T) {
	tests := []struct {
		name       string
		allow      bool
		err        error
		wantStatus int
		wantType   problem.Type
	}{
		{name: "allowed", allow: true, wantStatus: http.StatusNoContent},
		{name: "denied", wantStatus: http.StatusForbidden, wantType: problem.Forbidden},
		{name: "lookup fails", err: errors.New("cache not synced"), wantStatus: http.StatusInternalServerError, wantType: problem.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotProvider, gotUser string
			reg := NewRegistry()
			h, err := NewHandler(HandlerConfig{
				Registry:      reg,
				Authenticator: &tokenAuthenticator{users: map[string]string{"sveltos-token": sveltosUser}},
				Authorize: func(_ context.Context, provider, username string) (bool, error) {
					gotProvider, gotUser = provider, username
					return tt.allow, tt.err
				},
			})
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}

			req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(validBody(t, nil)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer sveltos-token")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantType != "" {
				if p := decodeProblem(t, rec); p.Type != tt.wantType {
					t.Errorf("problem type = %q, want %q", p.Type, tt.wantType)
				}
			}
			if gotProvider != "sveltos" || gotUser != sveltosUser {
				t.Errorf("Authorize(%q, %q), want (%q, %q)", gotProvider, gotUser, "sveltos", sveltosUser)
			}
			if _, registered := reg.Lookup("sveltos"); registered != (tt.wantStatus == http.StatusNoContent) {
				t.Errorf("registered = %t, want %t", registered, tt.wantStatus == http.StatusNoContent)
			}
		})
	}
}

func TestNewHandlerRejectsBothAllowListKinds(t *testing.T) {
	_, err := NewHandler(HandlerConfig{
		Registry:      NewRegistry(),
		Authenticator: &tokenAuthenticator{},
		Allowed:       map[string]string{"sveltos": sveltosUser},
		Authorize:     func(context.Context, string, string) (bool, error) { return true, nil },
	})
	if err == nil {
		t.Error("NewHandler succeeded with both Allowed and Authorize, want an error")
	}
}

func TestNilIsLeaderMeansAlwaysLeader(t *testing.T) {
	authn := &tokenAuthenticator{users: map[string]string{"sveltos-token": sveltosUser}}
	reg := NewRegistry()
	h, err := NewHandler(HandlerConfig{Registry: reg, Authenticator: authn, Allowed: map[string]string{"sveltos": sveltosUser}})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(validBody(t, nil)))
	req.Header.Set("Authorization", "Bearer sveltos-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestLeaderFromElected(t *testing.T) {
	elected := make(chan struct{})
	isLeader := LeaderFromElected(elected)

	if isLeader() {
		t.Error("isLeader = true before election")
	}
	close(elected)
	if !isLeader() {
		t.Error("isLeader = false after election")
	}
}

func TestHandlerAuthenticatorFailureIsInternal(t *testing.T) {
	reg := NewRegistry()
	h, err := NewHandler(HandlerConfig{
		Registry:      reg,
		Authenticator: failingAuthenticator{},
		Allowed:       map[string]string{"sveltos": sveltosUser},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, registerPath, strings.NewReader(validBody(t, nil)))
	req.Header.Set("Authorization", "Bearer sveltos-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

type failingAuthenticator struct{}

func (failingAuthenticator) Authenticate(context.Context, string) (string, error) {
	return "", errors.New("connection refused")
}
