// Package config defines and validates application configuration.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"whereiseveryone/pkg/env"
)

// Conservative defaults target low traffic on a small server; tune them together after measuring the deployment.
const (
	DefaultMaxConcurrentDBRequests        = 4
	DefaultDBRequestTimeout               = 15 * time.Second
	DefaultMongoMaxPoolSize        uint64 = 8
)

// DatabaseRequestLimits bounds admitted API requests and their total database-work budget.
type DatabaseRequestLimits struct {
	MaxRequests int
	Timeout     time.Duration
}

// Validate rejects settings that would disable admission or the request deadline.
func (l DatabaseRequestLimits) Validate() error {
	if l.MaxRequests <= 0 {
		return errors.New("app.maxConcurrentDBRequests must be a positive integer")
	}
	if l.Timeout <= 0 {
		return errors.New("app.dbRequestTimeoutSeconds must be a positive integer")
	}
	return nil
}

// DatabaseRequestLimitsFromEnv loads optional limits before the server starts.
func DatabaseRequestLimitsFromEnv(handler env.Handler) (DatabaseRequestLimits, error) {
	requests, err := strconv.Atoi(handler.Env(ConfMaxConcurrentDBRequests, strconv.Itoa(DefaultMaxConcurrentDBRequests)))
	if err != nil || requests <= 0 {
		return DatabaseRequestLimits{}, errors.New("app.maxConcurrentDBRequests must be a positive integer")
	}
	// A 32-bit count of seconds fits in time.Duration without overflowing.
	defaultSeconds := strconv.FormatInt(int64(DefaultDBRequestTimeout/time.Second), 10)
	seconds, err := strconv.ParseInt(handler.Env(ConfDBRequestTimeoutSeconds, defaultSeconds), 10, 32)
	if err != nil || seconds <= 0 {
		return DatabaseRequestLimits{}, errors.New("app.dbRequestTimeoutSeconds must be a positive integer")
	}
	return DatabaseRequestLimits{MaxRequests: requests, Timeout: time.Duration(seconds) * time.Second}, nil
}

// MongoMaxPoolSizeFromEnv loads a finite pool cap; zero would mean unlimited in the driver.
func MongoMaxPoolSizeFromEnv(handler env.Handler) (uint64, error) {
	defaultPoolSize := strconv.FormatUint(DefaultMongoMaxPoolSize, 10)
	poolSize, err := strconv.ParseUint(handler.Env(ConfMongoMaxPoolSize, defaultPoolSize), 10, 64)
	if err != nil || poolSize == 0 {
		return 0, fmt.Errorf("%s must be a positive integer", ConfMongoMaxPoolSize)
	}
	return poolSize, nil
}
