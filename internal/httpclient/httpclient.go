package httpclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
)

// New returns a TLS 1.2+ client trusting caBundle, or the system roots when caBundle is empty.
func New(caBundle []byte) (*http.Client, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(caBundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBundle) {
			return nil, errors.New("CA bundle contains no PEM certificates")
		}
		tlsConfig.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: transport}, nil
}
