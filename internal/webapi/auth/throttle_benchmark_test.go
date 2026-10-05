package auth

import (
	"fmt"
	"testing"
	"time"
)

func BenchmarkAuthThrottle(b *testing.B) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"admit", "reject", "full", "sweep_10000"} {
		b.Run(name, func(b *testing.B) {
			throttle := newAuthThrottle()
			q := newQuota(loginAccount, "alice", int(^uint(0)>>1), time.Hour)
			if name == "full" || name == "sweep_10000" {
				for i := range maxThrottleBuckets {
					key := newQuota(loginSource, fmt.Sprint(i), 1, time.Hour).key
					throttle.buckets[key] = attemptBudget{used: 1, expires: now.Add(time.Hour)}
				}
				throttle.lastSweep = now
			} else {
				if name == "reject" {
					q.limit = 1
				}
				if retry := throttle.allow(now, q); retry != 0 {
					b.Fatal(retry)
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				if name == "sweep_10000" {
					throttle.lastSweep = now.Add(-throttleSweepEvery)
				}
				throttle.allow(now, q)
			}
		})
	}
	b.Run("parallel_hot_account", func(b *testing.B) {
		throttle := newAuthThrottle()
		q := newQuota(loginAccount, "alice", int(^uint(0)>>1), time.Hour)
		b.ReportAllocs()
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				throttle.allow(now, q)
			}
		})
	})
}
