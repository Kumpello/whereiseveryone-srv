package crypto

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func BenchmarkHashPasswordWithCost(b *testing.B) {
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		if _, err := HashPasswordWithCost("benchmark-password", bcrypt.MinCost); err != nil {
			b.Fatalf("hash password: %v", err)
		}
	}
}

func BenchmarkVerifyPassword(b *testing.B) {
	hash, err := HashPasswordWithCost("benchmark-password", bcrypt.MinCost)
	if err != nil {
		b.Fatalf("hash password: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := VerifyPassword(hash, "benchmark-password"); err != nil {
			b.Fatalf("verify password: %v", err)
		}
	}
}
