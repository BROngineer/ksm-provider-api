package problem

import "net/http"

// maxForeignDetailBytes keeps a proxy's error page from flooding logs and conditions.
const maxForeignDetailBytes = 1 << 10

// FromResponse turns any non-2xx response into a Problem. Proxies and load balancers in front
// of a server answer with their own bodies, so a Problem is synthesized when the body is not one.
// It consumes the response body but does not close it.
func FromResponse(resp *http.Response) Problem {
	return Problem{}
}
