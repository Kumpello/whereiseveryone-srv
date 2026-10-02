package auth

import (
	"crypto/sha256"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"whereiseveryone/internal/webapi/jsonerr"
)

const (
	maxThrottleBuckets = 10_000
	throttleSweepEvery = time.Minute
	loginSourceLimit   = 30
	loginAccountLimit  = 10
	signupSourceLimit  = 5
	signupAccountLimit = 3
	signupGlobalLimit  = 100
)

type quotaKind uint8

const (
	loginSource quotaKind = iota
	loginAccount
	signupSource
	signupAccount
	signupGlobal
)

type quotaKey struct {
	kind   quotaKind
	digest [sha256.Size]byte
}

type quota struct {
	key    quotaKey
	limit  int
	window time.Duration
}

func newQuota(kind quotaKind, key string, limit int, window time.Duration) quota {
	return quota{quotaKey{kind, sha256.Sum256([]byte(key))}, limit, window}
}

type attemptBudget struct {
	used    int
	expires time.Time
}

type authThrottle struct {
	mu         sync.Mutex
	buckets    map[quotaKey]attemptBudget
	lastSweep  time.Time
	maxBuckets int
}

func newAuthThrottle() *authThrottle {
	return &authThrottle{buckets: make(map[quotaKey]attemptBudget), maxBuckets: maxThrottleBuckets}
}

// allow charges all quotas atomically, including attempts at nonexistent accounts.
// Active budgets are never evicted to admit new keys: rotating identifiers must
// not reset another account's or source's budget when the map reaches its cap.
func (t *authThrottle) allow(now time.Time, quotas ...quota) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !now.Before(t.lastSweep.Add(throttleSweepEvery)) {
		for key, budget := range t.buckets {
			if !now.Before(budget.expires) {
				delete(t.buckets, key)
			}
		}
		t.lastSweep = now
	}
	missing := 0
	var retry time.Duration
	for _, q := range quotas {
		budget, exists := t.buckets[q.key]
		if !exists {
			missing++
		}
		if now.Before(budget.expires) && budget.used >= q.limit {
			if remaining := budget.expires.Sub(now); remaining > retry {
				retry = remaining
			}
		}
	}
	if retry > 0 {
		return retry
	}
	if len(t.buckets)+missing > t.maxBuckets {
		return throttleSweepEvery
	}
	for _, q := range quotas {
		budget := t.buckets[q.key]
		if !now.Before(budget.expires) {
			budget = attemptBudget{expires: now.Add(q.window)}
		}
		budget.used++
		t.buckets[q.key] = budget
	}
	return 0
}

// authSource ignores forwarded headers. IPv4-mapped addresses share the IPv4
// budget, and IPv6 hosts in the same /64 share a budget to limit address rotation.
func authSource(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	addr = addr.WithZone("").Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().String()
	}
	return addr.String()
}

func tooManyRequests(c *echo.Context, retry time.Duration) error {
	seconds := (retry + time.Second - 1) / time.Second
	if seconds < 1 {
		seconds = 1
	}
	c.Response().Header().Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
	return c.JSON(http.StatusTooManyRequests, jsonerr.EchoError(http.StatusTooManyRequests, "too many requests", nil))
}

func (m *mux) throttleSource(signup bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			source := authSource(c.Request().RemoteAddr)
			quotas := []quota{newQuota(loginSource, source, loginSourceLimit, 5*time.Minute)}
			if signup {
				quotas = []quota{
					newQuota(signupSource, source, signupSourceLimit, time.Hour),
					newQuota(signupGlobal, "", signupGlobalLimit, time.Hour),
				}
			}
			if retry := m.throttle.allow(m.timer.Now(), quotas...); retry > 0 {
				return tooManyRequests(c, retry)
			}
			return next(c)
		}
	}
}
