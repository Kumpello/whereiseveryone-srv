package me

import (
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
