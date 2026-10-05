package users

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"whereiseveryone/pkg/id"
)

func TestMongoHotQueriesUseIndexes(t *testing.T) {
	ctx, db := newMongoTestDatabase(t)
	log := logrus.New()
	log.SetOutput(io.Discard)
	clock := authTestClock{time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	adapter := NewMongoAdapter(db.Collection("users"), db.Collection("pending_friend_requests"), clock, log)
	if err := adapter.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	const count = 256
	ids := make([]id.ID, count)
	documents := make([]any, 0, count)
	for i := range count {
		ids[i] = id.NewID()
		documents = append(documents, User{ID: ids[i], Auth: Auth{Username: fmt.Sprintf("user-%d", i)}, Location: &Location{LastUpdate: clock.now}})
	}
	if _, err := adapter.coll.InsertMany(ctx, documents); err != nil {
		t.Fatal(err)
	}
	requests := make([]any, 0, 2*(count-1))
	for _, userID := range ids[1:] {
		requests = append(requests,
			PendingFriendRequest{ID: id.NewID(), From: ids[0], To: userID},
			PendingFriendRequest{ID: id.NewID(), From: userID, To: ids[0]},
		)
	}
	if _, err := db.Collection("pending_friend_requests").InsertMany(ctx, requests); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, collection   string
		filter, projection bson.M
		returned           int
	}{
		{name: "session_by_id", collection: "users", filter: withUserId(ids[0]), returned: 1},
		{name: "login_by_username", collection: "users", filter: bson.M{"auth.username": "user-0"}, returned: 1},
		{name: "friends_batch", collection: "users", filter: bson.M{"_id": bson.M{"$in": ids[:16]}}, returned: 16},
		{name: "location_timestamp_guard", collection: "users", filter: bson.M{"_id": ids[0], "$or": bson.A{
			bson.M{"location.last_update": bson.M{"$exists": false}},
			bson.M{"location.last_update": bson.M{"$lt": clock.now.Add(time.Minute)}},
		}}, returned: 1},
		{name: "incoming_pending", collection: "pending_friend_requests", filter: bson.M{"to": ids[0]}, projection: bson.M{"_id": 0, "from": 1}, returned: count - 1},
		{name: "outgoing_pending", collection: "pending_friend_requests", filter: bson.M{"from": ids[0]}, projection: bson.M{"_id": 0, "to": 1}, returned: count - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			find := bson.D{{Key: "find", Value: tc.collection}, {Key: "filter", Value: tc.filter}}
			if tc.projection != nil {
				find = append(find, bson.E{Key: "projection", Value: tc.projection})
			}
			//nolint:tagliatelle // MongoDB explain responses have fixed camelCase field names.
			var result struct {
				ExecutionStats struct {
					Returned  int `bson:"nReturned"`
					Keys      int `bson:"totalKeysExamined"`
					Documents int `bson:"totalDocsExamined"`
				} `bson:"executionStats"`
				QueryPlanner struct {
					WinningPlan bson.M `bson:"winningPlan"`
				} `bson:"queryPlanner"`
			}
			command := bson.D{{Key: "explain", Value: find}, {Key: "verbosity", Value: "executionStats"}}
			if err := db.RunCommand(ctx, command).Decode(&result); err != nil {
				t.Fatal(err)
			}
			plan, err := bson.MarshalExtJSON(result.QueryPlanner.WinningPlan, false, false)
			if err != nil {
				t.Fatal(err)
			}
			stats := result.ExecutionStats
			if strings.Contains(string(plan), "COLLSCAN") || stats.Keys == 0 || stats.Documents > tc.returned || stats.Returned != tc.returned {
				t.Fatalf("query is not selective: stats=%+v plan=%s", stats, plan)
			}
			t.Logf("returned=%d keys=%d documents=%d plan=%s", stats.Returned, stats.Keys, stats.Documents, plan)
		})
	}
}

func newMongoBenchmarkDatabase(b *testing.B) (context.Context, *mongo.Database) {
	b.Helper()
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		b.Skip("set MONGO_TEST_URI to an isolated MongoDB instance")
	}
	ctx, cancel := context.WithTimeout(b.Context(), 5*time.Minute)
	b.Cleanup(cancel)
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(5 * time.Second))
	if err != nil {
		b.Fatal(err)
	}
	db := client.Database("users_performance_" + id.NewID().Hex())
	b.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if err := db.Drop(cleanupCtx); err != nil {
			b.Error(err)
		}
		if err := client.Disconnect(cleanupCtx); err != nil {
			b.Error(err)
		}
	})
	if err := client.Ping(ctx, nil); err != nil {
		b.Fatal(err)
	}
	return ctx, db
}

func BenchmarkMongoSessionRead(b *testing.B) {
	for _, count := range []int{0, 1000, 10000} {
		for _, projected := range []bool{false, true} {
			b.Run(fmt.Sprintf("friends=%d/projected=%t", count, projected), func(b *testing.B) {
				ctx, db := newMongoBenchmarkDatabase(b)
				clock := authTestClock{time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
				log := logrus.New()
				log.SetOutput(io.Discard)
				adapter := NewMongoAdapter(db.Collection("users"), db.Collection("pending_friend_requests"), clock, log)
				u := User{Auth: Auth{Username: "viewer", Token: "test-token", DeviceToken: "device-a"}, FriendSince: make(map[string]time.Time)}
				for range count {
					peerID := id.NewID()
					u.SubscribedUsers = append(u.SubscribedUsers, peerID)
					u.PausedUsers = append(u.PausedUsers, peerID)
					u.FriendSince[peerID.Hex()] = clock.now
				}
				var err error
				if u, err = adapter.NewUser(ctx, u); err != nil {
					b.Fatal(err)
				}
				encoded, err := bson.Marshal(u)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				for b.Loop() {
					if projected {
						session, err := adapter.GetSession(ctx, u.ID)
						if err != nil {
							b.Fatal(err)
						}
						if session.Token != u.Auth.Token {
							b.Fatal("projection lost the session token")
						}
					} else if _, err := adapter.GetUser(ctx, u.ID); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(len(encoded)), "stored_user_B")
			})
		}
	}
}

func BenchmarkMongoUpdateLocation(b *testing.B) {
	for _, name := range []string{"newer", "duplicate", "older"} {
		b.Run(name, func(b *testing.B) {
			ctx, db := newMongoBenchmarkDatabase(b)
			clock := authTestClock{time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
			log := logrus.New()
			log.SetOutput(io.Discard)
			adapter := NewMongoAdapter(db.Collection("users"), db.Collection("pending_friend_requests"), clock, log)
			u, err := adapter.NewUser(ctx, User{Auth: Auth{Username: "viewer"}})
			if err != nil {
				b.Fatal(err)
			}
			location := Location{Latitude: 52.23, Longitude: 21.01, LastUpdate: clock.now}
			if err := adapter.UpdateLocation(ctx, u.ID, location); err != nil {
				b.Fatal(err)
			}
			if name == "older" {
				location.LastUpdate = clock.now.Add(-time.Minute)
			}
			b.ReportAllocs()
			for b.Loop() {
				if name == "newer" {
					location.LastUpdate = location.LastUpdate.Add(time.Millisecond)
				}
				if err := adapter.UpdateLocation(ctx, u.ID, location); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
