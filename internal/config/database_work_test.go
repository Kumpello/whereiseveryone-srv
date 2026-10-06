package config

import (
	"testing"
	"time"

	"whereiseveryone/pkg/env"
)

type databaseWorkEnv map[env.Key]string

func (h databaseWorkEnv) Env(key env.Key, fallback string) string {
	if value, ok := h[key]; ok {
		return value
	}
	return fallback
}

func (h databaseWorkEnv) MustEnv(key env.Key) string { return h[key] }

func TestDatabaseWorkConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values databaseWorkEnv
		want   DatabaseRequestLimits
		pool   uint64
	}{
		{"defaults for existing configs", databaseWorkEnv{}, DatabaseRequestLimits{4, 15 * time.Second}, 8},
		{"overrides", databaseWorkEnv{
			ConfMaxConcurrentDBRequests: "2", ConfDBRequestTimeoutSeconds: "25", ConfMongoMaxPoolSize: "6",
		}, DatabaseRequestLimits{2, 25 * time.Second}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits, err := DatabaseRequestLimitsFromEnv(tc.values)
			if err != nil || limits != tc.want {
				t.Fatalf("request limits = %+v, error = %v; want %+v", limits, err, tc.want)
			}
			if validationErr := limits.Validate(); validationErr != nil {
				t.Fatal(validationErr)
			}
			pool, err := MongoMaxPoolSizeFromEnv(tc.values)
			if err != nil || pool != tc.pool {
				t.Fatalf("pool size = %d, error = %v; want %d", pool, err, tc.pool)
			}
		})
	}
}

func TestDatabaseWorkRejectsInvalidConfiguration(t *testing.T) {
	for _, key := range []env.Key{ConfMaxConcurrentDBRequests, ConfDBRequestTimeoutSeconds, ConfMongoMaxPoolSize} {
		for _, value := range []string{"", "0", "-1", "1.5", "unbounded", " 4", "9999999999999999999999999999999"} {
			t.Run(string(key)+"/"+value, func(t *testing.T) {
				handler := databaseWorkEnv{key: value}
				var err error
				if key == ConfMongoMaxPoolSize {
					_, err = MongoMaxPoolSizeFromEnv(handler)
				} else {
					_, err = DatabaseRequestLimitsFromEnv(handler)
				}
				if err == nil {
					t.Fatal("invalid limit was accepted")
				}
			})
		}
	}
	if _, err := DatabaseRequestLimitsFromEnv(databaseWorkEnv{ConfDBRequestTimeoutSeconds: "2147483648"}); err == nil {
		t.Fatal("timeout accepted seconds outside its supported integer range")
	}
	for _, limits := range []DatabaseRequestLimits{{}, {MaxRequests: -1, Timeout: time.Second}, {MaxRequests: 1}} {
		if err := limits.Validate(); err == nil {
			t.Fatalf("invalid programmatic limits accepted: %+v", limits)
		}
	}
}
