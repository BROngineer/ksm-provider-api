package auth

import (
	"context"
	"errors"
	"net/http"

	"k8s.io/client-go/kubernetes"
)

// ErrUnauthenticated marks a token the cluster rejected, as opposed to a failure to ask the cluster.
var ErrUnauthenticated = errors.New("unauthenticated")

type Authenticator interface {
	// Authenticate returns the Kubernetes username of the token's subject,
	// e.g. "system:serviceaccount:<namespace>:<name>".
	Authenticate(ctx context.Context, token string) (string, error)
}

func NewTokenReviewAuthenticator(cs kubernetes.Interface, audience string) Authenticator {
	return nil
}

// Middleware authenticates the bearer token and stores the username in the request context.
// Authorization is left to the handler or to RequireUser, because some decisions (registry
// names) depend on the request body.
func Middleware(a Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return next }
}

func RequireUser(allowed ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return next }
}

func UsernameFrom(ctx context.Context) (string, bool) {
	return "", false
}

func ServiceAccountUsername(namespace, name string) string {
	return ""
}
