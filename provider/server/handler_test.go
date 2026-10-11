package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/brongineer/ksm-provider-api/api/provider"
	"github.com/brongineer/ksm-provider-api/auth"
	"github.com/brongineer/ksm-provider-api/problem"
)

const (
	deployPath = "/v1/services:deploy"
	deletePath = "/v1/services:delete"
	kcmUser    = "system:serviceaccount:kcm-system:kcm-controller-manager"
	otherUser  = "system:serviceaccount:default:someone"
)

type stubProvider struct {
	deployResult provider.DeployServicesResult
	deployErr    error
	deleteResult provider.DeleteServicesResult
	deleteErr    error
	panicWith    any
	deployReqs   []provider.DeployServicesRequest
	deleteReqs   []provider.DeleteServicesRequest
}

func (s *stubProvider) DeployServices(_ context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error) {
	s.deployReqs = append(s.deployReqs, req)
	if s.panicWith != nil {
		panic(s.panicWith)
	}
	return s.deployResult, s.deployErr
}

func (s *stubProvider) DeleteServices(_ context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error) {
	s.deleteReqs = append(s.deleteReqs, req)
	return s.deleteResult, s.deleteErr
}

type tokenAuthenticator map[string]string

func (a tokenAuthenticator) Authenticate(_ context.Context, token string) (string, error) {
	if user, ok := a[token]; ok {
		return user, nil
	}
	return "", auth.ErrUnauthenticated
}

func newTestHandler(t *testing.T, p Provider) http.Handler {
	t.Helper()
	h, err := NewHandler(HandlerConfig{
		Provider:       p,
		Authenticator:  tokenAuthenticator{"kcm-token": kcmUser, "other-token": otherUser},
		AllowedCallers: []string{kcmUser},
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return h
}

func do(t *testing.T, h http.Handler, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const (
	deployBody = `{"name":"cd-1-abcd","namespace":"team-a","generation":7}`
	deleteBody = `{"name":"cd-1-abcd","namespace":"team-a"}`
)

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

func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, typ problem.Type) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, status, rec.Body.String())
	}
	if p := decodeProblem(t, rec); p.Type != typ {
		t.Errorf("problem type = %q, want %q", p.Type, typ)
	}
}

func deployedResult() provider.DeployServicesResult {
	return provider.DeployServicesResult{
		ObservedGeneration: 7,
		Applied:            true,
		Collected:          true,
		Services: []provider.ServiceState{{
			Name:         "ingress-nginx",
			Namespace:    "ingress",
			Template:     "ingress-nginx-4-11-0",
			Type:         provider.ServiceStateTypeHelm,
			State:        provider.ServiceStateStateDeployed,
			DeployedHash: new("abc123"),
		}},
	}
}

func TestDeployServicesSuccess(t *testing.T) {
	p := &stubProvider{deployResult: deployedResult()}

	rec := do(t, newTestHandler(t, p), deployPath, "kcm-token", deployBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	want := provider.DeployServicesRequest{Name: "cd-1-abcd", Namespace: "team-a", Generation: 7}
	if len(p.deployReqs) != 1 || p.deployReqs[0] != want {
		t.Errorf("provider requests = %+v, want [%+v]", p.deployReqs, want)
	}
	var got provider.DeployServicesResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got.ObservedGeneration != 7 || len(got.Services) != 1 || *got.Services[0].DeployedHash != "abc123" {
		t.Errorf("result = %+v, want the provider's result", got)
	}
}

func TestDeployFailureIsAResultNotAnError(t *testing.T) {
	p := &stubProvider{deployResult: provider.DeployServicesResult{
		ObservedGeneration: 7,
		Applied:            false,
		Reason:             new(provider.DeployServicesResultReasonSourceUnavailable),
		Message:            new("chart repository unreachable"),
	}}

	rec := do(t, newTestHandler(t, p), deployPath, "kcm-token", deployBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if services, ok := got["services"].([]any); !ok || len(services) != 0 {
		t.Errorf("services = %#v, want an empty array (nil slices must not become null)", got["services"])
	}
}

func TestDeployContractViolationsAreInternalErrors(t *testing.T) {
	tests := []struct {
		name   string
		result provider.DeployServicesResult
	}{
		{"not applied without reason", provider.DeployServicesResult{Applied: false, Message: new("boom")}},
		{"not applied without message", provider.DeployServicesResult{
			Applied: false, Reason: new(provider.DeployServicesResultReasonProviderError),
		}},
		{"unknown service state", func() provider.DeployServicesResult {
			r := deployedResult()
			r.Services[0].State = "Exploded"
			return r
		}()},
		{"unknown reason", provider.DeployServicesResult{
			Applied: false, Reason: new(provider.DeployServicesResultReason("Whatever")), Message: new("boom"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &stubProvider{deployResult: tt.result}
			rec := do(t, newTestHandler(t, p), deployPath, "kcm-token", deployBody)
			assertProblem(t, rec, http.StatusInternalServerError, problem.Internal)
		})
	}
}

func TestDeployErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantType   problem.Type
	}{
		{"not found", fmt.Errorf("get ServiceSet: %w", ErrNotFound), http.StatusNotFound, problem.NotFound},
		{"stale generation", fmt.Errorf("observed 6: %w", ErrStaleGeneration), http.StatusConflict, problem.StaleGeneration},
		{"anything else", errors.New("boom"), http.StatusInternalServerError, problem.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &stubProvider{deployErr: tt.err}
			rec := do(t, newTestHandler(t, p), deployPath, "kcm-token", deployBody)
			assertProblem(t, rec, tt.wantStatus, tt.wantType)
		})
	}
}

func TestInternalErrorsDoNotLeakDetails(t *testing.T) {
	const secret = "password=hunter2"
	p := &stubProvider{deployErr: errors.New("render values: " + secret)}

	rec := do(t, newTestHandler(t, p), deployPath, "kcm-token", deployBody)

	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("response leaks the provider error: %s", rec.Body.String())
	}
}

func TestProviderPanicIsInternalError(t *testing.T) {
	p := &stubProvider{panicWith: "nil map write"}

	rec := do(t, newTestHandler(t, p), deployPath, "kcm-token", deployBody)

	assertProblem(t, rec, http.StatusInternalServerError, problem.Internal)
	if strings.Contains(rec.Body.String(), "nil map") {
		t.Errorf("response leaks the panic value: %s", rec.Body.String())
	}
}

func TestDeleteServices(t *testing.T) {
	p := &stubProvider{deleteResult: provider.DeleteServicesResult{Done: true}}

	rec := do(t, newTestHandler(t, p), deletePath, "kcm-token", deleteBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	want := provider.DeleteServicesRequest{Name: "cd-1-abcd", Namespace: "team-a"}
	if len(p.deleteReqs) != 1 || p.deleteReqs[0] != want {
		t.Errorf("provider requests = %+v, want [%+v]", p.deleteReqs, want)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"done":true}` {
		t.Errorf("body = %s, want {\"done\":true}", rec.Body.String())
	}
}

func TestDeleteHasNoNotFoundOrConflictResponses(t *testing.T) {
	for _, err := range []error{ErrNotFound, ErrStaleGeneration} {
		p := &stubProvider{deleteErr: err}
		rec := do(t, newTestHandler(t, p), deletePath, "kcm-token", deleteBody)
		assertProblem(t, rec, http.StatusInternalServerError, problem.Internal)
	}
}

func TestRequestValidation(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		body      string
		wantField string
	}{
		{"malformed JSON", deployPath, `{"name":`, ""},
		{"missing generation", deployPath, `{"name":"a","namespace":"b"}`, "generation"},
		{"empty name", deployPath, `{"name":"","namespace":"b","generation":1}`, "name"},
		{"generation not an integer", deployPath, `{"name":"a","namespace":"b","generation":1.5}`, "generation"},
		{"delete without namespace", deletePath, `{"name":"a"}`, "namespace"},
		{"body too large", deployPath, `{"name":"` + strings.Repeat("a", maxRequestBytes) + `","namespace":"b","generation":1}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &stubProvider{deployResult: deployedResult()}

			rec := do(t, newTestHandler(t, p), tt.path, "kcm-token", tt.body)

			assertProblem(t, rec, http.StatusBadRequest, problem.InvalidRequest)
			if tt.wantField != "" {
				p := decodeProblem(t, rec)
				if !slices.ContainsFunc(p.Errors, func(e problem.FieldError) bool { return e.Field == tt.wantField }) {
					t.Errorf("errors = %+v, want an entry for %q", p.Errors, tt.wantField)
				}
			}
			if len(p.deployReqs)+len(p.deleteReqs) != 0 {
				t.Error("provider called with an invalid request")
			}
		})
	}
}

func TestCallerChecks(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		wantStatus int
		wantType   problem.Type
	}{
		{"missing token", "", http.StatusUnauthorized, problem.Unauthenticated},
		{"rejected token", "bogus", http.StatusUnauthorized, problem.Unauthenticated},
		{"caller is not kcm", "other-token", http.StatusForbidden, problem.Forbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, path := range []string{deployPath, deletePath} {
				p := &stubProvider{}
				rec := do(t, newTestHandler(t, p), path, tt.token, deployBody)
				assertProblem(t, rec, tt.wantStatus, tt.wantType)
				if len(p.deployReqs)+len(p.deleteReqs) != 0 {
					t.Errorf("%s: provider called for a rejected caller", path)
				}
			}
		})
	}
}

func TestNewHandlerRequiresDependencies(t *testing.T) {
	authn := tokenAuthenticator{}
	tests := []struct {
		name string
		cfg  HandlerConfig
	}{
		{"no provider", HandlerConfig{Authenticator: authn, AllowedCallers: []string{kcmUser}}},
		{"no authenticator", HandlerConfig{Provider: &stubProvider{}, AllowedCallers: []string{kcmUser}}},
		{"no allowed callers", HandlerConfig{Provider: &stubProvider{}, Authenticator: authn}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewHandler(tt.cfg); err == nil {
				t.Error("NewHandler succeeded, want an error")
			}
		})
	}
}
