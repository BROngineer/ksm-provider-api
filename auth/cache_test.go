package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"
)

type countingAuthenticator struct {
	user  string
	err   error
	calls map[string]int
}

func newCounting(user string, err error) *countingAuthenticator {
	return &countingAuthenticator{user: user, err: err, calls: map[string]int{}}
}

func (c *countingAuthenticator) Authenticate(_ context.Context, token string) (string, error) {
	c.calls[token]++
	if c.err != nil {
		return "", c.err
	}
	return c.user + "/" + token, nil
}

type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func newTestCache(inner Authenticator, ttl time.Duration) (*cachedAuthenticator, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	c := NewCachedAuthenticator(inner, ttl).(*cachedAuthenticator)
	c.now = clock.now
	return c, clock
}

func jwtWithExp(exp time.Time) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := enc.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, exp.Unix()))
	return header + "." + payload + ".signature"
}

func TestCachedAuthenticatorReusesResultWithinTTL(t *testing.T) {
	inner := newCounting("user", nil)
	c, clock := newTestCache(inner, time.Minute)

	first, err := c.Authenticate(context.Background(), "token-a")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	clock.advance(59 * time.Second)
	second, err := c.Authenticate(context.Background(), "token-a")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	if first != second {
		t.Errorf("cached user = %q, want %q", second, first)
	}
	if inner.calls["token-a"] != 1 {
		t.Errorf("inner calls = %d, want 1", inner.calls["token-a"])
	}
}

func TestCachedAuthenticatorExpiresAfterTTL(t *testing.T) {
	inner := newCounting("user", nil)
	c, clock := newTestCache(inner, time.Minute)

	_, _ = c.Authenticate(context.Background(), "token-a")
	clock.advance(time.Minute)
	_, _ = c.Authenticate(context.Background(), "token-a")

	if inner.calls["token-a"] != 2 {
		t.Errorf("inner calls = %d, want 2", inner.calls["token-a"])
	}
}

func TestCachedAuthenticatorKeysByToken(t *testing.T) {
	inner := newCounting("user", nil)
	c, _ := newTestCache(inner, time.Minute)

	a, _ := c.Authenticate(context.Background(), "token-a")
	b, _ := c.Authenticate(context.Background(), "token-b")

	if a == b {
		t.Errorf("different tokens returned the same user %q", a)
	}
	if inner.calls["token-a"] != 1 || inner.calls["token-b"] != 1 {
		t.Errorf("inner calls = %v, want one per token", inner.calls)
	}
}

func TestCachedAuthenticatorDoesNotCacheFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "rejected token", err: fmt.Errorf("%w: invalid", ErrUnauthenticated)},
		{name: "review call failure", err: errors.New("connection refused")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := newCounting("", tt.err)
			c, _ := newTestCache(inner, time.Minute)

			for range 3 {
				if _, err := c.Authenticate(context.Background(), "token-a"); !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want %v", err, tt.err)
				}
			}
			if inner.calls["token-a"] != 3 {
				t.Errorf("inner calls = %d, want 3", inner.calls["token-a"])
			}
		})
	}
}

func TestCachedAuthenticatorHonoursTokenExpiry(t *testing.T) {
	inner := newCounting("user", nil)
	c, clock := newTestCache(inner, time.Hour)
	token := jwtWithExp(clock.now().Add(10 * time.Second))

	_, _ = c.Authenticate(context.Background(), token)
	clock.advance(9 * time.Second)
	_, _ = c.Authenticate(context.Background(), token)
	if inner.calls[token] != 1 {
		t.Fatalf("inner calls before token expiry = %d, want 1", inner.calls[token])
	}

	clock.advance(time.Second)
	_, _ = c.Authenticate(context.Background(), token)
	if inner.calls[token] != 2 {
		t.Errorf("inner calls after token expiry = %d, want 2", inner.calls[token])
	}
}

func TestCachedAuthenticatorIgnoresUnparseableExpiry(t *testing.T) {
	inner := newCounting("user", nil)
	c, clock := newTestCache(inner, time.Minute)

	for _, token := range []string{"opaque-token", "a.!!!not-base64!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".c"} {
		_, _ = c.Authenticate(context.Background(), token)
		clock.advance(30 * time.Second)
		_, _ = c.Authenticate(context.Background(), token)
		if inner.calls[token] != 1 {
			t.Errorf("token %q: inner calls = %d, want 1 (TTL applies)", token, inner.calls[token])
		}
		clock.advance(time.Minute)
	}
}

func TestCachedAuthenticatorDoesNotStoreRawToken(t *testing.T) {
	c, _ := newTestCache(newCounting("user", nil), time.Minute)
	_, _ = c.Authenticate(context.Background(), "token-a")

	if c.len() != 1 {
		t.Fatalf("cache entries = %d, want 1", c.len())
	}
	if c.storesRawToken("token-a") {
		t.Error("cache key is the raw token, want a hash")
	}
}

func TestCachedAuthenticatorEvictsExpiredEntriesAtCapacity(t *testing.T) {
	c, clock := newTestCache(newCounting("user", nil), time.Minute)

	for i := range maxCacheEntries {
		_, _ = c.Authenticate(context.Background(), fmt.Sprintf("old-%d", i))
	}
	clock.advance(time.Minute)
	_, _ = c.Authenticate(context.Background(), "new")

	if c.len() != 1 {
		t.Errorf("cache entries = %d, want 1 (expired entries evicted)", c.len())
	}
}

func TestCachedAuthenticatorNeverExceedsCapacity(t *testing.T) {
	c, _ := newTestCache(newCounting("user", nil), time.Minute)

	for i := range maxCacheEntries + 10 {
		_, _ = c.Authenticate(context.Background(), fmt.Sprintf("live-%d", i))
	}

	if c.len() > maxCacheEntries {
		t.Errorf("cache entries = %d, want <= %d", c.len(), maxCacheEntries)
	}
}
