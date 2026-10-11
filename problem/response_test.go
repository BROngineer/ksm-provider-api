package problem

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func response(status int, contentType, body string) *http.Response {
	h := http.Header{}
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func TestFromResponseDecodesProblem(t *testing.T) {
	body := `{"type":"urn:ksm:problem:invalid-request","title":"Invalid request","status":400,` +
		`"detail":"bad","errors":[{"field":"name","message":"required"}]}`

	got := FromResponse(response(http.StatusBadRequest, ContentType, body))

	if got.Type != InvalidRequest || got.Title != "Invalid request" || got.Status != http.StatusBadRequest || got.Detail != "bad" {
		t.Errorf("problem = %+v, want the decoded body", got)
	}
	if len(got.Errors) != 1 || got.Errors[0].Field != "name" {
		t.Errorf("errors = %+v, want the decoded field error", got.Errors)
	}
}

func TestFromResponseAcceptsMediaTypeParameters(t *testing.T) {
	got := FromResponse(response(http.StatusNotFound, ContentType+"; charset=utf-8",
		`{"type":"urn:ksm:problem:not-found","title":"Not found","status":404}`))

	if got.Type != NotFound {
		t.Errorf("type = %q, want %q", got.Type, NotFound)
	}
}

func TestFromResponseFillsMissingStatus(t *testing.T) {
	got := FromResponse(response(http.StatusConflict, ContentType,
		`{"type":"urn:ksm:problem:stale-generation","title":"Stale generation"}`))

	if got.Status != http.StatusConflict {
		t.Errorf("status = %d, want %d", got.Status, http.StatusConflict)
	}
}

func TestFromResponseSynthesizesForForeignBodies(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		body        string
		wantInBody  string
	}{
		{"plain text from a proxy", http.StatusBadGateway, "text/plain", "upstream connect error", "upstream connect error"},
		{"HTML error page", http.StatusServiceUnavailable, "text/html", "<html>down</html>", "<html>down</html>"},
		{"problem content type with broken JSON", http.StatusInternalServerError, ContentType, `{"type":`, `{"type":`},
		{"problem content type without type", http.StatusInternalServerError, ContentType, `{"title":"x"}`, `{"title":"x"}`},
		{"empty body", http.StatusGatewayTimeout, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromResponse(response(tt.status, tt.contentType, tt.body))

			if got.Type != Internal {
				t.Errorf("type = %q, want %q", got.Type, Internal)
			}
			if got.Status != tt.status {
				t.Errorf("status = %d, want the response status %d", got.Status, tt.status)
			}
			if !strings.Contains(got.Detail, tt.wantInBody) {
				t.Errorf("detail = %q, want it to contain %q", got.Detail, tt.wantInBody)
			}
		})
	}
}

func TestFromResponseTruncatesForeignBodies(t *testing.T) {
	got := FromResponse(response(http.StatusBadGateway, "text/plain", strings.Repeat("x", 10<<10)))

	if len(got.Detail) > maxForeignDetailBytes+64 {
		t.Errorf("detail length = %d, want it truncated to about %d", len(got.Detail), maxForeignDetailBytes)
	}
}
