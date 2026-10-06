package config

import (
	"fmt"
	"strconv"

	"whereiseveryone/pkg/crypto"
	"whereiseveryone/pkg/env"

	"golang.org/x/crypto/bcrypt"
)

const (
	// One operation limits CPU contention on a small, low-traffic server.
	DefaultMaxConcurrentPasswordRequests = 1
	// Raising this ceiling requires deployment capacity and password-security review.
	MaxBcryptCost = 14
)

// PasswordWorkLimits controls signup hashing and login verification per instance.
type PasswordWorkLimits struct {
	HashCost    int
	MaxRequests int
}

// Validate rejects settings that disable admission or exceed the supported cost budget.
func (l PasswordWorkLimits) Validate() error {
	if l.HashCost < bcrypt.MinCost || l.HashCost > MaxBcryptCost {
		return fmt.Errorf("%s must be an integer in [%d,%d]", ConfBcryptCost, bcrypt.MinCost, MaxBcryptCost)
	}
	if l.MaxRequests <= 0 {
		return fmt.Errorf("%s must be a positive integer", ConfMaxConcurrentPasswordRequests)
	}
	return nil
}

// PasswordWorkLimitsFromEnv loads optional settings before database initialization or bcrypt work.
func PasswordWorkLimitsFromEnv(handler env.Handler) (PasswordWorkLimits, error) {
	cost, err := strconv.Atoi(handler.Env(ConfBcryptCost, strconv.Itoa(crypto.DefaultPasswordHashCost)))
	if err != nil {
		return PasswordWorkLimits{}, fmt.Errorf("%s must be an integer in [%d,%d]",
			ConfBcryptCost, bcrypt.MinCost, MaxBcryptCost)
	}
	defaultRequests := strconv.Itoa(DefaultMaxConcurrentPasswordRequests)
	requests, err := strconv.Atoi(handler.Env(ConfMaxConcurrentPasswordRequests, defaultRequests))
	if err != nil {
		return PasswordWorkLimits{}, fmt.Errorf("%s must be a positive integer", ConfMaxConcurrentPasswordRequests)
	}
	limits := PasswordWorkLimits{HashCost: cost, MaxRequests: requests}
	if err := limits.Validate(); err != nil {
		return PasswordWorkLimits{}, err
	}
	return limits, nil
}
