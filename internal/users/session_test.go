package users

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/v2/bson"

	"whereiseveryone/pkg/id"
)

func TestMongoGetSessionReadsOnlySessionFields(t *testing.T) {
	ctx, db := newMongoTestDatabase(t)
	clock := authTestClock{time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	log := logrus.New()
	log.SetOutput(io.Discard)
	adapter := NewMongoAdapter(db.Collection("users"), db.Collection("pending_friend_requests"), clock, log)
	want := Auth{
		Token: "current-access", DeviceToken: "device-a",
		PreviousAccessDigest: TokenDigest("previous-access"), PreviousAccessValidUntil: clock.now.Add(AccessRotationGrace),
	}
	user := User{
		Auth: want, Status: strings.Repeat("s", 1024),
		Location:    &Location{Latitude: 52, Longitude: 21, LastUpdate: clock.now},
		FriendSince: make(map[string]time.Time),
	}
	user.Auth.Username, user.Auth.Password = "alice", "private-password-hash"
	user.Auth.RefreshToken, user.Auth.RefreshTokenDigest = "legacy-refresh", TokenDigest("refresh")
	user.Auth.CreatedAt, user.Auth.UpdatedAt = clock.now, clock.now
	for range 10000 {
		peerID := id.NewID()
		user.SubscribedUsers = append(user.SubscribedUsers, peerID)
		user.PausedUsers = append(user.PausedUsers, peerID)
		user.FriendSince[peerID.Hex()] = clock.now
	}
	var err error
	user, err = adapter.NewUser(ctx, user)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("session fields only", func(t *testing.T) {
		got, err := adapter.GetSession(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("session = %+v, want only %+v", got, want)
		}
	})
	t.Run("full user remains available", func(t *testing.T) {
		got, err := adapter.GetUser(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, user) {
			t.Fatal("full user read lost profile, credentials, or relationship data")
		}
	})
	t.Run("missing user", func(t *testing.T) {
		if _, err := adapter.GetSession(ctx, id.NewID()); !errors.Is(err, ErrUserNotExists) {
			t.Fatalf("error = %v, want missing user", err)
		}
	})
	t.Run("canceled lookup", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := adapter.GetSession(canceled, user.ID); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want cancellation", err)
		}
	})
	t.Run("missing optional session fields", func(t *testing.T) {
		legacy, err := adapter.NewUser(ctx, User{Auth: Auth{Token: "legacy-access"}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := adapter.GetSession(ctx, legacy.ID)
		if err != nil || got != (Auth{Token: "legacy-access"}) {
			t.Fatalf("session = %+v, error = %v", got, err)
		}
	})
	t.Run("unrelated fields are not decoded", func(t *testing.T) {
		userID := id.NewID()
		if _, err := adapter.coll.InsertOne(ctx, bson.M{
			"_id": userID,
			"auth": bson.M{
				"token": want.Token, "device_token": want.DeviceToken,
				"previous_access_digest":      want.PreviousAccessDigest,
				"previous_access_valid_until": want.PreviousAccessValidUntil,
				"password":                    bson.M{"invalid_for_string": true},
			},
			"subscribed_users": "invalid_for_list",
		}); err != nil {
			t.Fatal(err)
		}
		got, err := adapter.GetSession(ctx, userID)
		if err != nil || got != want {
			t.Fatalf("unrelated fields affected session: session=%+v error=%v", got, err)
		}
	})
}

func TestMongoGetSessionObservesRotationAndRevocation(t *testing.T) {
	ctx, db := newMongoTestDatabase(t)
	clock := authTestClock{time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	log := logrus.New()
	log.SetOutput(io.Discard)
	adapter := NewMongoAdapter(db.Collection("users"), db.Collection("pending_friend_requests"), clock, log)
	user, err := adapter.NewUser(ctx, User{Auth: Auth{
		Token: "old-access", DeviceToken: "device-a", RefreshTokenDigest: TokenDigest("old-refresh"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = adapter.ReplaceTokens(ctx, user.ID, user.Auth, "new-access", "new-refresh", "device-a"); err != nil {
		t.Fatal(err)
	}
	session, err := adapter.GetSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.DeviceToken != "device-a" || !session.MatchesAccess("new-access", clock.now) ||
		!session.MatchesAccess("old-access", clock.now.Add(AccessRotationGrace-time.Millisecond)) ||
		session.MatchesAccess("old-access", clock.now.Add(AccessRotationGrace)) {
		t.Fatalf("session lost rotation or grace behavior: %+v", session)
	}
	clearAccess := ""
	if err = adapter.UpdateTokens(ctx, user.ID, &clearAccess, nil, nil); err != nil {
		t.Fatal(err)
	}
	session, err = adapter.GetSession(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Token != "" || session.PreviousAccessDigest != "" || !session.PreviousAccessValidUntil.IsZero() ||
		session.MatchesAccess("old-access", clock.now) || session.MatchesAccess("new-access", clock.now) {
		t.Fatal("revoked credentials remained valid on the next session read")
	}
}
