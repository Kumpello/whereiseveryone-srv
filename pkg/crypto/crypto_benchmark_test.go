package crypto

import (
	"fmt"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func BenchmarkHashPasswordWithCost(b *testing.B) {
	for _, cost := range []int{bcrypt.MinCost, 10, 12, DefaultPasswordHashCost} {
		b.Run(fmt.Sprintf("cost=%d", cost), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := HashPasswordWithCost("benchmark-password", cost); err != nil {
					b.Fatalf("hash password: %v", err)
				}
			}
		})
	}
}

func BenchmarkVerifyPassword(b *testing.B) {
	for _, cost := range []int{bcrypt.MinCost, 10, 12, DefaultPasswordHashCost} {
		b.Run(fmt.Sprintf("cost=%d", cost), func(b *testing.B) {
			hash, err := HashPasswordWithCost("benchmark-password", cost)
			if err != nil {
				b.Fatalf("hash password: %v", err)
			}
			b.ReportAllocs()
			for b.Loop() {
				if err := VerifyPassword(hash, "benchmark-password"); err != nil {
					b.Fatalf("verify password: %v", err)
				}
			}
		})
	}
}

func BenchmarkVerifyPasswordParallel(b *testing.B) {
	hash, err := HashPasswordWithCost("benchmark-password", DefaultPasswordHashCost)
	if err != nil {
		b.Fatalf("hash password: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := VerifyPassword(hash, "benchmark-password"); err != nil {
				b.Errorf("verify password: %v", err)
				return
			}
		}
	})
}
