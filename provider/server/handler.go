package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/brongineer/ksm-provider-api/api/provider"
	"github.com/brongineer/ksm-provider-api/auth"
	"github.com/brongineer/ksm-provider-api/internal/schema"
	"github.com/brongineer/ksm-provider-api/problem"
)

// maxRequestBytes is generous: requests carry only a ServiceSet key and a generation.
const maxRequestBytes = 16 << 10

// Provider is the only thing an adapter implements. Deployment failures are results, not
// errors: report them with Applied=false, a Reason and a Message.
type Provider interface {
	DeployServices(ctx context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error)
	DeleteServices(ctx context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error)
}

var (
	// ErrNotFound means the adapter cannot see the ServiceSet yet; kcm retries.
	ErrNotFound = errors.New("ServiceSet not found")
	// ErrStaleGeneration means the adapter read an older generation than requested; kcm retries.
	ErrStaleGeneration = errors.New("ServiceSet generation is behind the request")
)

var errNotAppliedWithoutCause = errors.New("applied=false requires both reason and message")

type HandlerConfig struct {
	Provider      Provider
	Authenticator auth.Authenticator
	Logger        *slog.Logger
	// AllowedCallers are the usernames allowed to call the provider, normally kcm's ServiceAccount.
	AllowedCallers []string
}

type handler struct {
	provider      Provider
	logger        *slog.Logger
	deployRequest *openapi3.Schema
	deployResult  *openapi3.Schema
	deleteRequest *openapi3.Schema
	deleteResult  *openapi3.Schema
}

func NewHandler(cfg HandlerConfig) (http.Handler, error) {
	if cfg.Provider == nil {
		return nil, errors.New("provider is required")
	}
	if cfg.Authenticator == nil {
		return nil, errors.New("authenticator is required")
	}
	if len(cfg.AllowedCallers) == 0 {
		return nil, errors.New("at least one allowed caller is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	h := &handler{provider: cfg.Provider, logger: logger}
	for name, dst := range map[string]**openapi3.Schema{
		"DeployServicesRequest": &h.deployRequest,
		"DeployServicesResult":  &h.deployResult,
		"DeleteServicesRequest": &h.deleteRequest,
		"DeleteServicesResult":  &h.deleteResult,
	} {
		s, err := schema.Load(provider.GetSpec, name)
		if err != nil {
			return nil, err
		}
		*dst = s
	}

	return provider.HandlerWithOptions(h, provider.StdHTTPServerOptions{
		// The generated wrapper applies middlewares in order, so the last one runs first.
		Middlewares: []provider.MiddlewareFunc{
			auth.RequireUser(cfg.AllowedCallers...),
			auth.Middleware(cfg.Authenticator),
			recoverPanics(logger),
		},
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, _ error) {
			problem.Write(w, problem.New(problem.InvalidRequest, "invalid request"))
		},
	}), nil
}

func (h *handler) DeployServices(w http.ResponseWriter, r *http.Request) {
	var req provider.DeployServicesRequest
	if !decodeRequest(w, r, h.deployRequest, &req) {
		return
	}
	res, err := h.provider.DeployServices(r.Context(), req)
	if err != nil {
		h.writeError(w, "deploy", err, true)
		return
	}
	// A nil slice would encode as null, which the schema rejects for a required array.
	if res.Services == nil {
		res.Services = []provider.ServiceState{}
	}
	if !res.Applied && (res.Reason == nil || res.Message == nil || *res.Message == "") {
		h.writeError(w, "deploy", errNotAppliedWithoutCause, false)
		return
	}
	h.writeResult(w, "deploy", h.deployResult, res)
}

func (h *handler) DeleteServices(w http.ResponseWriter, r *http.Request) {
	var req provider.DeleteServicesRequest
	if !decodeRequest(w, r, h.deleteRequest, &req) {
		return
	}
	res, err := h.provider.DeleteServices(r.Context(), req)
	if err != nil {
		// The delete operation declares no 404/409 responses, so every error is internal.
		h.writeError(w, "delete", err, false)
		return
	}
	h.writeResult(w, "delete", h.deleteResult, res)
}

func decodeRequest(w http.ResponseWriter, r *http.Request, s *openapi3.Schema, out any) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		problem.Write(w, problem.New(problem.InvalidRequest, "request body is unreadable or exceeds the size limit"))
		return false
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		problem.Write(w, problem.New(problem.InvalidRequest, "request body is not valid JSON"))
		return false
	}
	if fieldErrs := schema.Validate(s, doc); fieldErrs != nil {
		problem.Write(w, problem.New(problem.InvalidRequest, "request does not match the schema").WithErrors(fieldErrs...))
		return false
	}
	if err := json.Unmarshal(raw, out); err != nil {
		problem.Write(w, problem.New(problem.InvalidRequest, "request body cannot be decoded"))
		return false
	}
	return true
}

// writeError never puts err into the response: provider errors can carry values from Secrets.
func (h *handler) writeError(w http.ResponseWriter, op string, err error, retryable bool) {
	switch {
	case retryable && errors.Is(err, ErrNotFound):
		problem.Write(w, problem.New(problem.NotFound, "ServiceSet not found by the provider"))
	case retryable && errors.Is(err, ErrStaleGeneration):
		problem.Write(w, problem.New(problem.StaleGeneration, "provider read an older ServiceSet generation"))
	default:
		h.logger.Error("provider operation failed", "operation", op, "error", err)
		problem.Write(w, problem.New(problem.Internal, "provider failed to process the request"))
	}
}

// writeResult validates the result so a buggy adapter is caught here instead of in kcm.
func (h *handler) writeResult(w http.ResponseWriter, op string, s *openapi3.Schema, res any) {
	raw, err := json.Marshal(res)
	if err != nil {
		h.writeError(w, op, err, false)
		return
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		h.writeError(w, op, err, false)
		return
	}
	if fieldErrs := schema.Validate(s, doc); fieldErrs != nil {
		h.logger.Error("provider returned a result that violates the contract", "operation", op, "violations", fieldErrs)
		problem.Write(w, problem.New(problem.Internal, "provider failed to process the request"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func recoverPanics(logger *slog.Logger) provider.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					logger.Error("provider panicked", "panic", v, "path", r.URL.Path)
					problem.Write(w, problem.New(problem.Internal, "provider failed to process the request"))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
