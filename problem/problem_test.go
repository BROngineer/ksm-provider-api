package problem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNew(t *testing.T) {
	tests := []struct {
		typ        Type
		wantTitle  string
		wantStatus int
	}{
		{InvalidRequest, "Invalid request", http.StatusBadRequest},
		{Unauthenticated, "Unauthenticated", http.StatusUnauthorized},
		{Forbidden, "Forbidden", http.StatusForbidden},
		{NotFound, "Not found", http.StatusNotFound},
		{StaleGeneration, "Stale generation", http.StatusConflict},
		{Internal, "Internal error", http.StatusInternalServerError},
		{NotLeader, "Not leader", http.StatusServiceUnavailable},
		{"urn:ksm:problem:unknown", "Internal error", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(string(tt.typ), func(t *testing.T) {
			p := New(tt.typ, "some detail")
			if p.Type != tt.typ {
				t.Errorf("Type = %q, want %q", p.Type, tt.typ)
			}
			if p.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", p.Title, tt.wantTitle)
			}
			if p.Status != tt.wantStatus {
				t.Errorf("Status = %d, want %d", p.Status, tt.wantStatus)
			}
			if p.Detail != "some detail" {
				t.Errorf("Detail = %q, want %q", p.Detail, "some detail")
			}
		})
	}
}

func TestWithErrors(t *testing.T) {
	base := New(InvalidRequest, "bad")
	got := base.WithErrors(FieldError{Field: "ttlSeconds", Message: "must be >= 1"}, FieldError{Field: "name", Message: "required"})

	if len(got.Errors) != 2 {
		t.Fatalf("len(Errors) = %d, want 2", len(got.Errors))
	}
	if got.Errors[0].Field != "ttlSeconds" || got.Errors[1].Field != "name" {
		t.Errorf("Errors = %+v, want ttlSeconds then name", got.Errors)
	}
	if len(base.Errors) != 0 {
		t.Errorf("base problem mutated: Errors = %+v", base.Errors)
	}
}

func TestError(t *testing.T) {
	if got, want := New(NotFound, "servicesets/a not found").Error(), "Not found: servicesets/a not found"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got, want := New(Internal, "").Error(), "Internal error"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, New(InvalidRequest, "bad").WithErrors(FieldError{Field: "name", Message: "required"}))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if got := rec.Header().Get("Content-Type"); got != ContentType {
		t.Errorf("Content-Type = %q, want %q", got, ContentType)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	want := map[string]any{
		"type":   string(InvalidRequest),
		"title":  "Invalid request",
		"status": float64(http.StatusBadRequest),
		"detail": "bad",
		"errors": []any{map[string]any{"field": "name", "message": "required"}},
	}
	for k, v := range want {
		if gotJSON, wantJSON := mustJSON(t, body[k]), mustJSON(t, v); gotJSON != wantJSON {
			t.Errorf("body[%q] = %s, want %s", k, gotJSON, wantJSON)
		}
	}
}

func TestWriteOmitsEmptyOptionalFields(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, New(Internal, ""))

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	for _, k := range []string{"detail", "errors"} {
		if _, ok := body[k]; ok {
			t.Errorf("body contains %q, want it omitted", k)
		}
	}
}

func TestWriteZeroStatusFallsBackToInternal(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, Problem{Type: Internal, Title: "Internal error"})

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
