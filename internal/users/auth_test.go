package users

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"whereiseveryone/pkg/id"
)

type authTestClock struct{ now time.Time }

func (c authTestClock) Now() time.Time { return c.now }

func TestRefreshDigestMatching(t *testing.T) {
	for _, tc := range []struct {
		name  string
		auth  Auth
		token string
		want  bool
	}{
		{"digest", Auth{RefreshTokenDigest: TokenDigest("credential")}, "credential", true},
		{"wrong credential", Auth{RefreshTokenDigest: TokenDigest("credential")}, "wrong", false},
		{"legacy", Auth{RefreshToken: "credential"}, "credential", true},
		{"no plaintext fallback", Auth{RefreshToken: "old", RefreshTokenDigest: TokenDigest("new")}, "old", false},
		{"empty", Auth{}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.auth.MatchesRefresh(tc.token); got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
}

// Run with MONGO_TEST_URI pointing at an isolated MongoDB instance.
func TestMongoRefreshRotation(t *testing.T) {
	ctx, db := newMongoTestDatabase(t)
	clock := authTestClock{time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	adapter := mongoAuthAdapter{coll: db.Collection("users"), timer: clock}
	read := func(userID id.ID) Auth {
		t.Helper()
		var user User
		if err := adapter.coll.FindOne(ctx, withUserId(userID)).Decode(&user); err != nil {
			t.Fatal(err)
		}
		return user.Auth
	}
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			userID := id.NewID()
			previous := Auth{Token: "old-access", DeviceToken: "device-a", RefreshTokenDigest: TokenDigest("old-refresh")}
			if legacy {
				previous.RefreshTokenDigest, previous.RefreshToken = "", "old-refresh"
			}
			if _, err := adapter.coll.InsertOne(ctx, User{ID: userID, Auth: previous}); err != nil {
				t.Fatal(err)
			}
			const contenders = 16
			var ready sync.WaitGroup
			ready.Add(contenders)
			type outcome struct {
				index int
				err   error
			}
			results := make(chan outcome, contenders)
			for i := range contenders {
				go func() {
					ready.Done()
					ready.Wait()
					results <- outcome{i, adapter.ReplaceTokens(ctx, userID, previous, fmt.Sprintf("access-%d", i), fmt.Sprintf("refresh-%d", i), "device-a")}
				}()
			}
			winner, wins := -1, 0
			for range contenders {
				result := <-results
				if result.err == nil {
					winner = result.index
					wins++
				} else if !errors.Is(result.err, ErrSessionChanged) {
					t.Fatal(result.err)
				}
			}
			if wins != 1 {
				t.Fatalf("successful atomic rotations = %d, want 1", wins)
			}
			current := read(userID)
			if current.Token != fmt.Sprintf("access-%d", winner) || !current.MatchesRefresh(fmt.Sprintf("refresh-%d", winner)) || current.RefreshToken != "" {
				t.Fatal("database did not persist only the winner and its refresh digest")
			}
			var raw bson.Raw
			if err := adapter.coll.FindOne(ctx, withUserId(userID)).Decode(&raw); err != nil {
				t.Fatal(err)
			}
			if raw.Lookup("auth", "refresh_token").Type != 0 {
				t.Fatal("plaintext refresh field was not removed")
			}
			if current.PreviousAccessDigest != TokenDigest(previous.Token) || !current.PreviousAccessValidUntil.Equal(clock.now.Add(120*time.Second)) {
				t.Fatal("rotation did not persist the bounded access grace")
			}
			if err := adapter.ReplaceTokens(ctx, userID, previous, "replay-access", "replay-refresh", "device-a"); !errors.Is(err, ErrSessionChanged) {
				t.Fatalf("replay error = %v", err)
			}
			// A stale request, including a device-conflict revocation, cannot overwrite a login.
			access, refresh, device := "login-access", "login-refresh", "device-b"
			if err := adapter.UpdateTokens(ctx, userID, &access, &refresh, &device); err != nil {
				t.Fatal(err)
			}
			for _, incomingDevice := range []string{"device-a", ""} {
				if err := adapter.ReplaceTokens(ctx, userID, current, "stale-access", "stale-refresh", incomingDevice); !errors.Is(err, ErrSessionChanged) {
					t.Fatalf("stale session error = %v", err)
				}
			}
			current = read(userID)
			if current.Token != access || !current.MatchesRefresh(refresh) || current.RefreshToken != "" || current.PreviousAccessDigest != "" || !current.PreviousAccessValidUntil.IsZero() {
				t.Fatal("login must store a digest, clear grace, and survive stale requests")
			}
			// A current conflict revokes both current and grace credentials atomically.
			if err := adapter.ReplaceTokens(ctx, userID, current, "next-access", "next-refresh", device); err != nil {
				t.Fatal(err)
			}
			current = read(userID)
			if err := adapter.ReplaceTokens(ctx, userID, current, "revoked-access", "revoked-refresh", ""); err != nil {
				t.Fatal(err)
			}
			current = read(userID)
			if current.DeviceToken != "" || current.PreviousAccessDigest != "" || !current.PreviousAccessValidUntil.IsZero() {
				t.Fatal("conflict must clear device and grace")
			}
		})
	}
	t.Run("no matched user", func(t *testing.T) {
		access, refresh := "access", "refresh"
		if err := adapter.UpdateTokens(ctx, id.NewID(), &access, &refresh, nil); !errors.Is(err, ErrSessionChanged) {
			t.Fatalf("unmatched update error = %v", err)
		}
		if err := adapter.ReplaceTokens(ctx, id.NewID(), Auth{}, access, refresh, "device-a"); !errors.Is(err, ErrSessionChanged) {
			t.Fatalf("unmatched rotation error = %v", err)
		}
	})
}
