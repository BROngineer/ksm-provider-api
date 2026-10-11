package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/brongineer/ksm-provider-api/problem"
)

const (
	testAudience = "ksm-registry"
	testUser     = "system:serviceaccount:projectsveltos:ksm-provider-sveltos"
)

type reviewReply struct {
	status authenticationv1.TokenReviewStatus
	err    error
}

func fakeClientset(t *testing.T, reply reviewReply, gotSpec *authenticationv1.TokenReviewSpec) *fake.Clientset {
	t.Helper()
	cs := fake.NewClientset()
	cs.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		if gotSpec != nil {
			*gotSpec = review.Spec
		}
		if reply.err != nil {
			return true, nil, reply.err
		}
		out := review.DeepCopy()
		out.Status = reply.status
		return true, out, nil
	})
	return cs
}

func TestTokenReviewAuthenticator(t *testing.T) {
	tests := []struct {
		name      string
		reply     reviewReply
		wantUser  string
		wantUnaut bool
		wantErr   bool
	}{
		{
			name: "authenticated with matching audience",
			reply: reviewReply{status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				User:          authenticationv1.UserInfo{Username: testUser},
				Audiences:     []string{testAudience},
			}},
			wantUser: testUser,
		},
		{
			name:      "rejected token",
			reply:     reviewReply{status: authenticationv1.TokenReviewStatus{Authenticated: false, Error: "invalid token"}},
			wantUnaut: true,
			wantErr:   true,
		},
		{
			name: "authenticated for another audience",
			reply: reviewReply{status: authenticationv1.TokenReviewStatus{
				Authenticated: true,
				User:          authenticationv1.UserInfo{Username: testUser},
				Audiences:     []string{"kubernetes"},
			}},
			wantUnaut: true,
			wantErr:   true,
		},
		{
			name:    "review call fails",
			reply:   reviewReply{err: errors.New("connection refused")},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var spec authenticationv1.TokenReviewSpec
			a := NewTokenReviewAuthenticator(fakeClientset(t, tt.reply, &spec), testAudience)

			user, err := a.Authenticate(context.Background(), "the-token")

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got := errors.Is(err, ErrUnauthenticated); got != tt.wantUnaut {
				t.Errorf("errors.Is(err, ErrUnauthenticated) = %v, want %v (err: %v)", got, tt.wantUnaut, err)
			}
			if user != tt.wantUser {
				t.Errorf("user = %q, want %q", user, tt.wantUser)
			}
			if spec.Token != "the-token" {
				t.Errorf("review token = %q, want %q", spec.Token, "the-token")
			}
			if len(spec.Audiences) != 1 || spec.Audiences[0] != testAudience {
				t.Errorf("review audiences = %v, want [%s]", spec.Audiences, testAudience)
			}
		})
	}
}

type stubAuthenticator struct {
	user string
	err  error
	got  string
}

func (s *stubAuthenticator) Authenticate(_ context.Context, token string) (string, error) {
	s.got = token
	return s.user, s.err
}

func TestMiddleware(t *testing.T) {
	tests := []struct {
		name        string
		header      string
		auth        *stubAuthenticator
		wantStatus  int
		wantType    problem.Type
		wantToken   string
		wantNextHit bool
	}{
		{
			name:        "valid bearer token",
			header:      "Bearer the-token",
			auth:        &stubAuthenticator{user: testUser},
			wantStatus:  http.StatusOK,
			wantToken:   "the-token",
			wantNextHit: true,
		},
		{
			name:        "scheme is case-insensitive",
			header:      "bearer the-token",
			auth:        &stubAuthenticator{user: testUser},
			wantStatus:  http.StatusOK,
			wantToken:   "the-token",
			wantNextHit: true,
		},
		{
			name:       "missing header",
			auth:       &stubAuthenticator{user: testUser},
			wantStatus: http.StatusUnauthorized,
			wantType:   problem.Unauthenticated,
		},
		{
			name:       "non-bearer scheme",
			header:     "Basic dXNlcjpwYXNz",
			auth:       &stubAuthenticator{user: testUser},
			wantStatus: http.StatusUnauthorized,
			wantType:   problem.Unauthenticated,
		},
		{
			name:       "empty token",
			header:     "Bearer ",
			auth:       &stubAuthenticator{user: testUser},
			wantStatus: http.StatusUnauthorized,
			wantType:   problem.Unauthenticated,
		},
		{
			name:       "token rejected",
			header:     "Bearer the-token",
			auth:       &stubAuthenticator{err: ErrUnauthenticated},
			wantStatus: http.StatusUnauthorized,
			wantType:   problem.Unauthenticated,
			wantToken:  "the-token",
		},
		{
			name:       "authenticator failure is internal, not 401",
			header:     "Bearer the-token",
			auth:       &stubAuthenticator{err: errors.New("connection refused")},
			wantStatus: http.StatusInternalServerError,
			wantType:   problem.Internal,
			wantToken:  "the-token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var nextHit bool
			var gotUser string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextHit = true
				gotUser, _ = UsernameFrom(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			Middleware(tt.auth)(next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if nextHit != tt.wantNextHit {
				t.Errorf("next handler called = %v, want %v", nextHit, tt.wantNextHit)
			}
			if tt.auth.got != tt.wantToken {
				t.Errorf("token passed to authenticator = %q, want %q", tt.auth.got, tt.wantToken)
			}
			if tt.wantNextHit && gotUser != testUser {
				t.Errorf("UsernameFrom = %q, want %q", gotUser, testUser)
			}
			if tt.wantType != "" {
				assertProblem(t, rec, tt.wantType)
			}
		})
	}
}

func TestMiddlewareDoesNotLeakAuthenticatorError(t *testing.T) {
	const secret = "dial tcp 10.0.0.1:6443: secret-internal-detail"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer the-token")

	Middleware(&stubAuthenticator{err: errors.New(secret)})(http.NotFoundHandler()).ServeHTTP(rec, req)

	if body := rec.Body.String(); strings.Contains(body, secret) {
		t.Errorf("response body leaks authenticator error: %s", body)
	}
}

func TestRequireUser(t *testing.T) {
	allowed := ServiceAccountUsername("kcm-system", "kcm-controller-manager")
	tests := []struct {
		name       string
		user       string
		setUser    bool
		wantStatus int
	}{
		{name: "allowed user", user: allowed, setUser: true, wantStatus: http.StatusOK},
		{name: "other user", user: testUser, setUser: true, wantStatus: http.StatusForbidden},
		{name: "no authenticated user", wantStatus: http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
			h := RequireUser(allowed)(next)
			if tt.setUser {
				h = Middleware(&stubAuthenticator{user: tt.user})(h)
			}

			req := httptest.NewRequest(http.MethodPost, "/", nil)
			req.Header.Set("Authorization", "Bearer the-token")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusForbidden {
				assertProblem(t, rec, problem.Forbidden)
			}
		})
	}
}

func TestServiceAccountUsername(t *testing.T) {
	if got, want := ServiceAccountUsername("kcm-system", "kcm"), "system:serviceaccount:kcm-system:kcm"; got != want {
		t.Errorf("ServiceAccountUsername = %q, want %q", got, want)
	}
}

func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, want problem.Type) {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", got, problem.ContentType)
	}
	var p problem.Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not a problem: %v", err)
	}
	if p.Type != want {
		t.Errorf("problem type = %q, want %q", p.Type, want)
	}
}
