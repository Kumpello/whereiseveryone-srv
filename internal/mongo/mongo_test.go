package mongo

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestPoolLimitOverridesURI(t *testing.T) {
	for _, tc := range []struct {
		name  string
		uri   string
		sizes []uint64
		want  uint64
	}{
		{"default", "mongodb://localhost:27017", nil, 8},
		{"unlimited URI", "mongodb://localhost:27017/?maxPoolSize=0", nil, 8},
		{"large URI", "mongodb://localhost:27017/?maxPoolSize=100", nil, 8},
		{"explicit limit", "mongodb://localhost:27017/?maxPoolSize=100&minPoolSize=1", []uint64{3}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := options.Client().ApplyURI(tc.uri)
			if err := applyPoolLimit(opts, tc.sizes...); err != nil {
				t.Fatal(err)
			}
			if opts.MaxPoolSize == nil || *opts.MaxPoolSize != tc.want {
				t.Fatalf("max pool size = %v, want %d", opts.MaxPoolSize, tc.want)
			}
		})
	}
}

func TestPoolLimitRejectsUnboundedOrConflictingOptions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		uri   string
		sizes []uint64
	}{
		{"unlimited size", "mongodb://localhost:27017", []uint64{0}},
		{"multiple limits", "mongodb://localhost:27017", []uint64{4, 8}},
		{"URI minimum exceeds cap", "mongodb://localhost:27017/?minPoolSize=9", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := applyPoolLimit(options.Client().ApplyURI(tc.uri), tc.sizes...); err == nil {
				t.Fatal("invalid pool options were accepted")
			}
		})
	}
}
