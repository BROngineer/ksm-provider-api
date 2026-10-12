package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/brongineer/ksm-provider-api/problem"
)

// Audiences of the projected ServiceAccount tokens. Distinct audiences keep a token stolen from
// one direction from being replayed in the other; both sides' Helm charts must use these values.
const (
	// RegistryAudience is requested by adapters for heartbeats and checked by the kcm registry.
	RegistryAudience = "ksm-registry"
	// ProviderAudience is requested by kcm for provider calls and checked by adapters.
	ProviderAudience = "ksm-provider"
)

// ErrUnauthenticated marks a token the cluster rejected, as opposed to a failure to ask the cluster.
var ErrUnauthenticated = errors.New("unauthenticated")

type Authenticator interface {
	// Authenticate returns the Kubernetes username of the token's subject,
	// e.g. "system:serviceaccount:<namespace>:<name>".
	Authenticate(ctx context.Context, token string) (string, error)
}

type tokenReviewAuthenticator struct {
	cs       kubernetes.Interface
	audience string
}

func NewTokenReviewAuthenticator(cs kubernetes.Interface, audience string) Authenticator {
	return &tokenReviewAuthenticator{cs: cs, audience: audience}
}

func (a *tokenReviewAuthenticator) Authenticate(ctx context.Context, token string) (string, error) {
	review := &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{
			Token:     token,
			Audiences: []string{a.audience},
		},
	}
	res, err := a.cs.AuthenticationV1().TokenReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create TokenReview: %w", err)
	}
	if !res.Status.Authenticated {
		return "", fmt.Errorf("%w: %s", ErrUnauthenticated, res.Status.Error)
	}
	// The API server may authenticate a token for other audiences; only ours proves the
	// token was minted for this API.
	if !slices.Contains(res.Status.Audiences, a.audience) {
		return "", fmt.Errorf("%w: token not issued for audience %q", ErrUnauthenticated, a.audience)
	}
	return res.Status.User.Username, nil
}

type usernameKey struct{}

// Middleware authenticates the bearer token and stores the username in the request context.
// Authorization is left to the handler or to RequireUser, because some decisions (registry
// names) depend on the request body.
func Middleware(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				problem.Write(w, problem.New(problem.Unauthenticated, "missing bearer token"))
				return
			}
			user, err := a.Authenticate(r.Context(), token)
			switch {
			case errors.Is(err, ErrUnauthenticated):
				problem.Write(w, problem.New(problem.Unauthenticated, "token rejected"))
				return
			case err != nil:
				// The cause may expose cluster internals, so it stays out of the response.
				problem.Write(w, problem.New(problem.Internal, "authentication unavailable"))
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), usernameKey{}, user)))
		})
	}
}

func RequireUser(allowed ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := UsernameFrom(r.Context())
			if !ok || !slices.Contains(allowed, user) {
				problem.Write(w, problem.New(problem.Forbidden, "caller is not allowed"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func UsernameFrom(ctx context.Context) (string, bool) {
	user, ok := ctx.Value(usernameKey{}).(string)
	return user, ok
}

func ServiceAccountUsername(namespace, name string) string {
	return "system:serviceaccount:" + namespace + ":" + name
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
