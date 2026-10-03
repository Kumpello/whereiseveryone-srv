package users

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"whereiseveryone/pkg/id"
)

func newMongoTestDatabase(t *testing.T) (context.Context, *mongo.Database) {
	t.Helper()
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("set MONGO_TEST_URI to run MongoDB regression tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatal(err)
	}
	db := client.Database("users_regression_" + id.NewID().Hex())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if err := db.Drop(cleanupCtx); err != nil {
			t.Error(err)
		}
		if err := client.Disconnect(cleanupCtx); err != nil {
			t.Error(err)
		}
	})
	if err := client.Ping(ctx, nil); err != nil {
		t.Fatal(err)
	}
	return ctx, db
}

func TestMongoLocationOnlyReplacesOlderFixes(t *testing.T) {
	ctx, db := newMongoTestDatabase(t)
	log := logrus.New()
	log.SetOutput(io.Discard)
	clock := authTestClock{time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	adapter := NewMongoAdapter(db.Collection("users"), db.Collection("pending_friend_requests"), clock, log)
	user, err := adapter.NewUser(ctx, User{Auth: Auth{Username: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	first := Location{Longitude: 21, Latitude: 52, LastUpdate: clock.now}
	if err := adapter.UpdateLocation(ctx, user.ID, first); err != nil {
		t.Fatal(err)
	}
	assertLocation := func(want Location) {
		t.Helper()
		stored, err := adapter.GetUser(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Location == nil || *stored.Location != want {
			t.Fatalf("stored location = %+v, want %+v", stored.Location, want)
		}
	}
	for _, offset := range []time.Duration{-time.Minute, 0} {
		if err := adapter.UpdateLocation(ctx, user.ID, Location{Longitude: 99, LastUpdate: clock.now.Add(offset)}); err != nil {
			t.Fatal(err)
		}
		assertLocation(first)
	}

	const updates = 32
	errs := make(chan error, updates)
	var workers sync.WaitGroup
	for n := 1; n <= updates; n++ {
		workers.Go(func() {
			errs <- adapter.UpdateLocation(ctx, user.ID, Location{Longitude: float64(n), LastUpdate: clock.now.Add(time.Duration(n) * time.Millisecond)})
		})
	}
	workers.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertLocation(Location{Longitude: updates, LastUpdate: clock.now.Add(updates * time.Millisecond)})

	if err := adapter.WipeLocation(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.UpdateLocation(ctx, user.ID, first); err != nil {
		t.Fatal(err)
	}
	assertLocation(first)
}

func TestMongoUserOperations(t *testing.T) {
	ctx, db := newMongoTestDatabase(t)
	log := logrus.New()
	log.SetOutput(io.Discard)
	clock := authTestClock{time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	adapter := NewMongoAdapter(db.Collection("users"), db.Collection("pending_friend_requests"), clock, log)
	for range 2 {
		if err := adapter.EnsureIndexes(ctx); err != nil {
			t.Fatal(err)
		}
	}
	create := func(name string) User {
		t.Helper()
		user, err := adapter.NewUser(ctx, User{Auth: Auth{Username: name, Password: "private-hash"}, SubscribedUsers: []id.ID{}, PausedUsers: []id.ID{}})
		if err != nil {
			t.Fatal(err)
		}
		return user
	}
	alice, bob := create("alice"), create("bob")
	if _, err := adapter.NewUser(ctx, User{Auth: Auth{Username: "alice"}}); !errors.Is(err, ErrUserNameAlreadyExists) {
		t.Fatalf("duplicate username error = %v", err)
	}
	for range 2 {
		if err := adapter.SendFriendRequest(ctx, alice.ID, bob.ID); err != nil {
			t.Fatal(err)
		}
	}
	incoming, incomingErr := adapter.GetPendingIncomingFriendRequestUserIDs(ctx, bob.ID)
	if incomingErr != nil || len(incoming) != 1 || incoming[0] != alice.ID {
		t.Fatalf("incoming requests = %v, error = %v", incoming, incomingErr)
	}
	outgoing, outgoingErr := adapter.GetPendingOutgoingFriendRequestUserIDs(ctx, alice.ID)
	if outgoingErr != nil || len(outgoing) != 1 || outgoing[0] != bob.ID {
		t.Fatalf("outgoing requests = %v, error = %v", outgoing, outgoingErr)
	}
	if err := adapter.AcceptFriendRequest(ctx, bob.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]id.ID{{alice.ID, bob.ID}, {bob.ID, alice.ID}} {
		user, err := adapter.GetUser(ctx, pair[0])
		if err != nil {
			t.Fatal(err)
		}
		since := user.FriendSinceFor(pair[1])
		if !user.SubscribeUser(pair[1]) || since == nil || !since.Equal(clock.now) {
			t.Fatal("friendship bulk update did not persist")
		}
	}
	location := Location{Longitude: 21.01, Latitude: 52.23, LastUpdate: clock.now}
	if err := adapter.UpdateLocation(ctx, alice.ID, location); err != nil {
		t.Fatal(err)
	}
	projected, projectionErr := adapter.GetUsers(ctx, []id.ID{alice.ID})
	if projectionErr != nil {
		t.Fatal(projectionErr)
	}
	if len(projected) != 1 || projected[0].Location == nil || *projected[0].Location != location || projected[0].Auth.Password != "" {
		t.Fatal("projection must include location and exclude credentials")
	}
	if err := adapter.WipeLocation(ctx, alice.ID); err != nil {
		t.Fatal(err)
	}
	storedAlice, readErr := adapter.GetUser(ctx, alice.ID)
	if readErr != nil || storedAlice.Location != nil {
		t.Fatalf("location wipe error = %v", readErr)
	}
	if err := adapter.UnfriendUser(ctx, alice.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	storedAlice, readErr = adapter.GetUserByUsername(ctx, "alice")
	if readErr != nil || storedAlice.SubscribeUser(bob.ID) || storedAlice.FriendSinceFor(bob.ID) != nil {
		t.Fatalf("unfriend error = %v", readErr)
	}
}
