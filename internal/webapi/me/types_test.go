package me

import (
	"encoding/json"
	"testing"
	"time"
)

func TestFriendDetailsFriendSince(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)

	accepted := newFriendDetails("alice", "hello", friendStateAccepted, &now)
	if accepted.FriendSince == nil || !accepted.FriendSince.Equal(now) {
		t.Fatalf("accepted friend should carry the provided friend since time")
	}

	pending := newFriendDetails("bob", "hello", friendStatePendingIncoming, nil)
	if pending.FriendSince != nil {
		t.Fatalf("pending friend should not carry a friend since time")
	}
}

func TestTimestampJSONRoundTrip(t *testing.T) {
	now := time.UnixMilli(1719859200000).UTC()

	encoded, err := json.Marshal(timestamp(now))
	if err != nil {
		t.Fatalf("marshal timestamp: %v", err)
	}

	if string(encoded) != "1719859200000" {
		t.Fatalf("expected unix milliseconds timestamp, got %s", string(encoded))
	}

	var decoded timestamp
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal timestamp: %v", err)
	}

	if decoded != timestamp(now) {
		t.Fatalf("expected round-tripped time %v, got %v", now, decoded)
	}
}
