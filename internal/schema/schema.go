package schema

import (
	"errors"
	"fmt"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/brongineer/ksm-provider-api/problem"
)

// Load returns a named component schema from an embedded spec.
func Load(spec func() (*openapi3.T, error), name string) (*openapi3.Schema, error) {
	t, err := spec()
	if err != nil {
		return nil, fmt.Errorf("load embedded spec: %w", err)
	}
	ref, ok := t.Components.Schemas[name]
	if !ok || ref.Value == nil {
		return nil, fmt.Errorf("embedded spec has no %s schema", name)
	}
	return ref.Value, nil
}

// Validate reports every violation as a field error. Reasons from kin-openapi never include
// the rejected value, so they are safe to return to the caller.
func Validate(s *openapi3.Schema, doc any) []problem.FieldError {
	err := s.VisitJSON(doc, openapi3.MultiErrors())
	if err == nil {
		return nil
	}
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
