package problem

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// maxForeignDetailBytes keeps a proxy's error page from flooding logs and conditions.
const maxForeignDetailBytes = 1 << 10

// maxProblemBytes bounds how much of an error body is read at all.
const maxProblemBytes = 64 << 10

// FromResponse turns any non-2xx response into a Problem. Proxies and load balancers in front
// of a server answer with their own bodies, so a Problem is synthesized when the body is not one.
// It consumes the response body but does not close it.
func FromResponse(resp *http.Response) Problem {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxProblemBytes))

	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && mediaType == ContentType {
		var p Problem
		if err := json.Unmarshal(body, &p); err == nil && p.Type != "" {
			if p.Status == 0 {
				p.Status = resp.StatusCode
			}
			return p
		}
	}

	p := New(Internal, foreignDetail(body))
	p.Status = resp.StatusCode
	return p
}

func foreignDetail(body []byte) string {
	detail := strings.TrimSpace(string(body))
	if len(detail) > maxForeignDetailBytes {
		detail = detail[:maxForeignDetailBytes] + "... (truncated)"
	}
	return detail
}
