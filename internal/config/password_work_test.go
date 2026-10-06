package config

import (
	"strings"
	"testing"

	"whereiseveryone/pkg/env"
)

func TestPasswordWorkConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values databaseWorkEnv
		want   PasswordWorkLimits
	}{
		{"defaults for existing configs", databaseWorkEnv{}, PasswordWorkLimits{HashCost: 14, MaxRequests: 1}},
		{"concurrency override", databaseWorkEnv{ConfMaxConcurrentPasswordRequests: "2"}, PasswordWorkLimits{HashCost: 14, MaxRequests: 2}},
		{"cost override", databaseWorkEnv{ConfBcryptCost: "12"}, PasswordWorkLimits{HashCost: 12, MaxRequests: 1}},
		{"minimum supported cost", databaseWorkEnv{ConfBcryptCost: "4", ConfMaxConcurrentPasswordRequests: "3"}, PasswordWorkLimits{HashCost: 4, MaxRequests: 3}},
		{"maximum supported cost", databaseWorkEnv{ConfBcryptCost: "14"}, PasswordWorkLimits{HashCost: 14, MaxRequests: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits, err := PasswordWorkLimitsFromEnv(tc.values)
			if err != nil || limits != tc.want {
				t.Fatalf("password limits = %+v, error = %v; want %+v", limits, err, tc.want)
			}
		})
	}
}

func TestPasswordWorkRejectsInvalidConfiguration(t *testing.T) {
	for _, key := range []env.Key{ConfBcryptCost, ConfMaxConcurrentPasswordRequests} {
		for _, value := range []string{"", "0", "-1", "1.5", "unbounded", " 4", "9999999999999999999999999999999"} {
			t.Run(string(key)+"/"+value, func(t *testing.T) {
				_, err := PasswordWorkLimitsFromEnv(databaseWorkEnv{key: value})
				if err == nil || !strings.Contains(err.Error(), string(key)) {
					t.Fatalf("invalid setting must identify %s: %v", key, err)
				}
			})
		}
	}
	for _, cost := range []string{"3", "15", "31", "32"} {
		if _, err := PasswordWorkLimitsFromEnv(databaseWorkEnv{ConfBcryptCost: cost}); err == nil {
			t.Fatalf("unsupported bcrypt cost %s was accepted", cost)
		}
	}
	for _, limits := range []PasswordWorkLimits{
		{}, {HashCost: 14, MaxRequests: 0}, {HashCost: 14, MaxRequests: -1},
		{HashCost: 3, MaxRequests: 1}, {HashCost: 15, MaxRequests: 1},
	} {
		if err := limits.Validate(); err == nil {
			t.Fatalf("invalid programmatic limits accepted: %+v", limits)
		}
	}
}
