package problem

import "net/http"

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

func New(t Type, detail string) Problem {
	return Problem{}
}

func (p Problem) WithErrors(errs ...FieldError) Problem {
	return p
}

func (p Problem) Error() string {
	return ""
}

func Write(w http.ResponseWriter, p Problem) {}
