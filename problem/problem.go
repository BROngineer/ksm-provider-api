package problem

import (
	"encoding/json"
	"net/http"
	"slices"
)

const ContentType = "application/problem+json"

// Type is the stable identifier clients branch on; title and detail are for humans only.
type Type string

const (
	InvalidRequest  Type = "urn:ksm:problem:invalid-request"
	Unauthenticated Type = "urn:ksm:problem:unauthenticated"
	Forbidden       Type = "urn:ksm:problem:forbidden"
	NotFound        Type = "urn:ksm:problem:not-found"
	StaleGeneration Type = "urn:ksm:problem:stale-generation"
	Internal        Type = "urn:ksm:problem:internal"
	NotLeader       Type = "urn:ksm:problem:not-leader"
)

type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// Problem is wire-compatible with the Problem schema of both the provider and the registry specs.
type Problem struct {
	Type   Type         `json:"type"`
	Title  string       `json:"title"`
	Status int          `json:"status"`
	Detail string       `json:"detail,omitempty"`
	Errors []FieldError `json:"errors,omitempty"`
}

type meta struct {
	title  string
	status int
}

var known = map[Type]meta{
	InvalidRequest:  {"Invalid request", http.StatusBadRequest},
	Unauthenticated: {"Unauthenticated", http.StatusUnauthorized},
	Forbidden:       {"Forbidden", http.StatusForbidden},
	NotFound:        {"Not found", http.StatusNotFound},
	StaleGeneration: {"Stale generation", http.StatusConflict},
	Internal:        {"Internal error", http.StatusInternalServerError},
	NotLeader:       {"Not leader", http.StatusServiceUnavailable},
}

// New keeps an unknown type as-is but answers it as an internal error, so a typo never turns into a 200.
func New(t Type, detail string) Problem {
	m, ok := known[t]
	if !ok {
		m = known[Internal]
	}
	return Problem{Type: t, Title: m.title, Status: m.status, Detail: detail}
}

func (p Problem) WithErrors(errs ...FieldError) Problem {
	p.Errors = append(slices.Clone(p.Errors), errs...)
	return p
}

func (p Problem) Error() string {
	if p.Detail == "" {
		return p.Title
	}
	return p.Title + ": " + p.Detail
}

func Write(w http.ResponseWriter, p Problem) {
	if p.Status == 0 {
		p.Status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(p.Status)
	// The status line is already sent; an encoding failure can only mean a broken connection.
	_ = json.NewEncoder(w).Encode(p)
}
