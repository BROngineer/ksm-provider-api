package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// maxCacheEntries bounds memory: projected tokens rotate, so old entries pile up otherwise.
const maxCacheEntries = 1024

type cacheKey [sha256.Size]byte

type cacheEntry struct {
	expiresAt time.Time
	username  string
}

type cachedAuthenticator struct {
	inner   Authenticator
	now     func() time.Time
	entries map[cacheKey]cacheEntry
	ttl     time.Duration
	mu      sync.Mutex
}

// NewCachedAuthenticator caches successful authentications for ttl, or until the token's
// own expiry if that comes first. Rejections and errors are never cached.
func NewCachedAuthenticator(inner Authenticator, ttl time.Duration) Authenticator {
	return &cachedAuthenticator{
		inner:   inner,
		ttl:     ttl,
		now:     time.Now,
		entries: make(map[cacheKey]cacheEntry),
	}
}

func (c *cachedAuthenticator) Authenticate(ctx context.Context, token string) (string, error) {
	key := sha256.Sum256([]byte(token))
	now := c.now()

	c.mu.Lock()
	entry, ok := c.entries[key]
	c.mu.Unlock()
	if ok && now.Before(entry.expiresAt) {
		return entry.username, nil
	}

	username, err := c.inner.Authenticate(ctx, token)
	if err != nil {
		return "", err
	}

	expiresAt := now.Add(c.ttl)
	if exp, ok := tokenExpiry(token); ok && exp.Before(expiresAt) {
		expiresAt = exp
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists && len(c.entries) >= maxCacheEntries {
		c.evict(now)
	}
	c.entries[key] = cacheEntry{username: username, expiresAt: expiresAt}
	return username, nil
}

// evict drops expired entries and, if the cache is still full, the entry closest to expiry.
func (c *cachedAuthenticator) evict(now time.Time) {
	var (
		soonestKey cacheKey
		soonest    time.Time
	)
	for k, e := range c.entries {
		if !now.Before(e.expiresAt) {
			delete(c.entries, k)
			continue
		}
		if soonest.IsZero() || e.expiresAt.Before(soonest) {
			soonestKey, soonest = k, e.expiresAt
		}
	}
	if len(c.entries) >= maxCacheEntries {
		delete(c.entries, soonestKey)
	}
}

// tokenExpiry reads the exp claim without verifying the signature; the TokenReview already
// established that the token is genuine, so this only bounds how long the answer is reused.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

func (c *cachedAuthenticator) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *cachedAuthenticator) storesRawToken(token string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.entries {
		if string(k[:]) == token {
			return true
		}
	}
	return false
}
