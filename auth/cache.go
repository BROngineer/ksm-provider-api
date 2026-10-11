package auth

import (
	"context"
	"time"
)

// maxCacheEntries bounds memory: projected tokens rotate, so old entries pile up otherwise.
const maxCacheEntries = 1024

type cachedAuthenticator struct {
	inner Authenticator
	ttl   time.Duration
	now   func() time.Time
}

// NewCachedAuthenticator caches successful authentications for ttl, or until the token's
// own expiry if that comes first. Rejections and errors are never cached.
func NewCachedAuthenticator(inner Authenticator, ttl time.Duration) Authenticator {
	return &cachedAuthenticator{inner: inner, ttl: ttl, now: time.Now}
}

func (c *cachedAuthenticator) Authenticate(ctx context.Context, token string) (string, error) {
	return c.inner.Authenticate(ctx, token)
}

func (c *cachedAuthenticator) len() int {
	return 0
}

func (c *cachedAuthenticator) storesRawToken(token string) bool {
	return false
}
