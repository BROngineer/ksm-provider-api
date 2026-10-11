package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/brongineer/ksm-provider-api/api/registry"
	"github.com/brongineer/ksm-provider-api/auth"
	"github.com/brongineer/ksm-provider-api/problem"
)

// maxRequestBytes leaves room for a CA bundle of a few certificates.
const maxRequestBytes = 64 << 10

const defaultTimeout = 30

type HandlerConfig struct {
	Registry      *Registry
	Authenticator auth.Authenticator
	// Allowed maps a provider name to the only ServiceAccount username that may register it.
	Allowed map[string]string
	// IsLeader reports whether this replica holds the registry. Nil means always.
	IsLeader func() bool
	Logger   *slog.Logger
}

type registerHandler struct {
	registry *Registry
	schema   *openapi3.Schema
	logger   *slog.Logger
	allowed  map[string]string
}

func NewHandler(cfg HandlerConfig) (http.Handler, error) {
	if cfg.Registry == nil {
		return nil, errors.New("registry is required")
	}
	if cfg.Authenticator == nil {
		return nil, errors.New("authenticator is required")
	}

	spec, err := registry.GetSpec()
	if err != nil {
		return nil, fmt.Errorf("load embedded registry spec: %w", err)
	}
	schemaRef, ok := spec.Components.Schemas["RegisterProviderRequest"]
	if !ok || schemaRef.Value == nil {
		return nil, errors.New("embedded registry spec has no RegisterProviderRequest schema")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	isLeader := cfg.IsLeader
	if isLeader == nil {
		isLeader = func() bool { return true }
	}

	h := &registerHandler{
		registry: cfg.Registry,
		schema:   schemaRef.Value,
		logger:   logger,
		allowed:  cfg.Allowed,
	}
	return registry.HandlerWithOptions(h, registry.StdHTTPServerOptions{
		// The generated wrapper applies middlewares in order, so the last one runs first:
		// the leader gate answers non-leaders before any TokenReview is spent.
		Middlewares: []registry.MiddlewareFunc{
			auth.Middleware(cfg.Authenticator),
			leaderGate(isLeader),
		},
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, _ error) {
			problem.Write(w, problem.New(problem.InvalidRequest, "invalid request"))
		},
	}), nil
}

func (h *registerHandler) RegisterKSMProvider(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		problem.Write(w, problem.New(problem.InvalidRequest, "request body is unreadable or exceeds the size limit"))
		return
	}

	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		problem.Write(w, problem.New(problem.InvalidRequest, "request body is not valid JSON"))
		return
	}
	if err := h.schema.VisitJSON(doc, openapi3.MultiErrors()); err != nil {
		problem.Write(w, problem.New(problem.InvalidRequest, "request does not match the schema").WithErrors(fieldErrors(err)...))
		return
	}

	var req registry.RegisterProviderRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		problem.Write(w, problem.New(problem.InvalidRequest, "request body cannot be decoded"))
		return
	}
	if fe, ok := validateEndpoint(req.Endpoint); !ok {
		problem.Write(w, problem.New(problem.InvalidRequest, "request does not match the schema").WithErrors(fe))
		return
	}

	user, _ := auth.UsernameFrom(r.Context())
	if allowed, ok := h.allowed[req.Name]; !ok || allowed != user {
		h.logger.Info("rejected provider registration", "provider", req.Name, "user", user)
		problem.Write(w, problem.New(problem.Forbidden, "caller is not allowed to register this provider name"))
		return
	}

	h.registry.Register(toRegistration(req))
	w.WriteHeader(http.StatusNoContent)
}

func toRegistration(req registry.RegisterProviderRequest) Registration {
	reg := Registration{
		Name:        req.Name,
		Endpoint:    req.Endpoint,
		State:       State(req.State),
		APIVersions: req.ApiVersions,
		TTL:         time.Duration(req.TtlSeconds) * time.Second,
		Timeout:     defaultTimeout * time.Second,
	}
	if req.CaBundle != nil {
		reg.CABundle = *req.CaBundle
	}
	if req.TimeoutSeconds != nil {
		reg.Timeout = time.Duration(*req.TimeoutSeconds) * time.Second
	}
	return reg
}

// validateEndpoint exists because the schema's "uri" format is not enforced by the validator.
func validateEndpoint(endpoint string) (problem.FieldError, bool) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return problem.FieldError{Field: "endpoint", Message: "must be an absolute http or https URL"}, false
	}
	return problem.FieldError{}, true
}

func fieldErrors(err error) []problem.FieldError {
	var out []problem.FieldError
	var visit func(error)
	visit = func(err error) {
		if multi, ok := errors.AsType[openapi3.MultiError](err); ok {
			for _, e := range multi {
				visit(e)
			}
			return
		}
		if schemaErr, ok := errors.AsType[*openapi3.SchemaError](err); ok {
			out = append(out, problem.FieldError{
				Field:   strings.Join(schemaErr.JSONPointer(), "."),
				Message: schemaErr.Reason,
			})
			return
		}
		out = append(out, problem.FieldError{Message: "invalid value"})
	}
	visit(err)
	return out
}

func leaderGate(isLeader func() bool) registry.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isLeader() {
				problem.Write(w, problem.New(problem.NotLeader, "this replica is not the leader; retry"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// LeaderFromElected adapts controller-runtime's Manager.Elected channel.
func LeaderFromElected(elected <-chan struct{}) func() bool {
	return func() bool {
		select {
		case <-elected:
			return true
		default:
			return false
		}
	}
}
