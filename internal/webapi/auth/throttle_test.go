package auth

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountBudgetExpiresWithoutResettingOnRejection(t *testing.T) {
	now := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	throttle := newAuthThrottle()
	q := newQuota(loginAccount, "alice", 2, time.Minute)
	for range 2 {
		if retry := throttle.allow(now, q); retry != 0 {
			t.Fatalf("allowed attempt returned retry = %v", retry)
		}
	}
	if retry := throttle.allow(now.Add(time.Second), q); retry != 59*time.Second {
		t.Fatalf("exhausted budget returned retry = %v", retry)
	}
	if retry := throttle.allow(now.Add(time.Minute-time.Millisecond), q); retry != time.Millisecond {
		t.Fatalf("rejection extended the budget: retry = %v", retry)
	}
	if retry := throttle.allow(now.Add(time.Minute), q); retry != 0 {
		t.Fatalf("expired budget was not reusable: %v", retry)
	}
}

func TestQuotaAdmissionIsAtomic(t *testing.T) {
	now := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	throttle := newAuthThrottle()
	global := newQuota(signupGlobal, "", 1, time.Hour)
	first := newQuota(signupSource, "first", 1, time.Hour)
	second := newQuota(signupSource, "second", 1, time.Hour)
	if retry := throttle.allow(now, first, global); retry != 0 {
		t.Fatal(retry)
	}
	if retry := throttle.allow(now, second, global); retry != time.Hour {
		t.Fatalf("global quota did not reject: %v", retry)
	}
	if _, exists := throttle.buckets[second.key]; exists {
		t.Fatal("rejected request consumed a different quota")
	}
}

func TestThrottleMemoryIsBoundedWithoutEvictingLiveBudgets(t *testing.T) {
	now := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	throttle := newAuthThrottle()
	throttle.maxBuckets = 16
	for i := range throttle.maxBuckets {
		if retry := throttle.allow(now, newQuota(loginAccount, fmt.Sprint(i), 1, 15*time.Minute)); retry != 0 {
			t.Fatal(retry)
		}
	}
	for i := range 100 {
		if retry := throttle.allow(now, newQuota(loginAccount, fmt.Sprint(i+16), 1, 15*time.Minute)); retry == 0 {
			t.Fatal("new identity admitted with full throttle storage")
		}
	}
	if len(throttle.buckets) != 16 {
		t.Fatalf("throttle grew to %d entries", len(throttle.buckets))
	}
	if retry := throttle.allow(now, newQuota(loginAccount, "0", 1, 15*time.Minute)); retry != 15*time.Minute {
		t.Fatalf("live budget was evicted: %v", retry)
	}
	if retry := throttle.allow(now.Add(15*time.Minute), newQuota(loginAccount, "new", 1, time.Minute)); retry != 0 {
		t.Fatalf("expired entries did not free storage: %v", retry)
	}
	if len(throttle.buckets) != 1 {
		t.Fatalf("expired buckets were retained: %d entries", len(throttle.buckets))
	}
}

func TestConcurrentAttemptsCannotExceedBudget(t *testing.T) {
	now := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	throttle := newAuthThrottle()
	q := newQuota(loginAccount, "alice", loginAccountLimit, time.Minute)
	var allowed atomic.Int32
	var done sync.WaitGroup
	for range 100 {
		done.Go(func() {
			if throttle.allow(now, q) == 0 {
				allowed.Add(1)
			}
		})
	}
	done.Wait()
	if got := allowed.Load(); got != loginAccountLimit {
		t.Fatalf("concurrent attempts admitted = %d, want %d", got, loginAccountLimit)
	}
}

func TestAuthSourceCanonicalization(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"192.0.2.1:1234", "192.0.2.1"},
		{"192.0.2.1:9999", "192.0.2.1"},
		{"[::ffff:192.0.2.1]:1234", "192.0.2.1"},
		{"[2001:db8:1:2::1]:1234", "2001:db8:1:2::/64"},
		{"[2001:db8:1:2::abcd]:9999", "2001:db8:1:2::/64"},
		{"[fe80::1%eth0]:1234", "fe80::/64"},
		{"", "unknown"},
		{"invalid", "unknown"},
	} {
		t.Run(tc.remote, func(t *testing.T) {
			if got := authSource(tc.remote); got != tc.want {
				t.Fatalf("source = %q, want %q", got, tc.want)
			}
		})
	}
}
