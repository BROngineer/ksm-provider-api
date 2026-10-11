package schema

import (
	"errors"
	"slices"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/brongineer/ksm-provider-api/api/registry"
	"github.com/brongineer/ksm-provider-api/problem"
)

func TestLoad(t *testing.T) {
	if _, err := Load(registry.GetSpec, "RegisterProviderRequest"); err != nil {
		t.Errorf("Load existing schema: %v", err)
	}
	if _, err := Load(registry.GetSpec, "NoSuchSchema"); err == nil {
		t.Error("Load missing schema succeeded, want an error")
	}
	failing := func() (*openapi3.T, error) { return nil, errors.New("broken spec") }
	if _, err := Load(failing, "RegisterProviderRequest"); err == nil {
		t.Error("Load with a failing spec succeeded, want an error")
	}
}

func TestValidate(t *testing.T) {
	s, err := Load(registry.GetSpec, "RegisterProviderRequest")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	valid := map[string]any{
		"name": "sveltos", "endpoint": "https://a.svc", "state": "Active",
		"ttlSeconds": float64(30), "apiVersions": []any{"v1"},
	}
	if got := Validate(s, valid); got != nil {
		t.Errorf("Validate(valid) = %+v, want nil", got)
	}

	invalid := map[string]any{
		"name": "sveltos", "endpoint": "https://a.svc", "state": "Paused",
		"ttlSeconds": float64(0), "apiVersions": []any{"v1"},
	}
	got := Validate(s, invalid)
	for _, field := range []string{"state", "ttlSeconds"} {
		if !slices.ContainsFunc(got, func(e problem.FieldError) bool { return e.Field == field && e.Message != "" }) {
			t.Errorf("Validate(invalid) = %+v, want an entry for %q", got, field)
		}
	}
}
