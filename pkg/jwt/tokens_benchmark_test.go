package jwt

import (
	"testing"
	"time"

	"whereiseveryone/pkg/id"
)

type benchmarkTimer struct {
	now time.Time
}

func (b benchmarkTimer) Now() time.Time {
	return b.now
}

func BenchmarkGenerateTokens(b *testing.B) {
	j := NewJWT(
		benchmarkTimer{now: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)},
		[]byte("benchmark-secret"),
		15*time.Minute,
		720*time.Hour,
	)
	userID := id.NewID()

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := j.GenerateTokens("benchmark-user", userID); err != nil {
			b.Fatalf("generate tokens: %v", err)
		}
	}
}

func BenchmarkValidateToken(b *testing.B) {
	j := NewJWT(
		benchmarkTimer{now: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)},
		[]byte("benchmark-secret"),
		15*time.Minute,
		720*time.Hour,
	)
	userID := id.NewID()
	token, _, err := j.GenerateTokens("benchmark-user", userID)
	if err != nil {
		b.Fatalf("generate token: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := j.ValidateToken(token); err != nil {
			b.Fatalf("validate token: %v", err)
		}
	}
}
