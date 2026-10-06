package users

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"whereiseveryone/pkg/id"
)

// BenchmarkMongoListProjections compares the old batch query with actual list reads.
// returned_BSON_B is the sum of document bytes, excluding protocol overhead.
func BenchmarkMongoListProjections(b *testing.B) {
	for _, pausedCount := range []int{0, 1000, 10000} {
		for _, mode := range []string{"legacy", "accepted", "names"} {
			b.Run(fmt.Sprintf("peers=50/paused=%d/%s", pausedCount, mode), func(b *testing.B) {
				ctx, db := newMongoBenchmarkDatabase(b)
				clock := authTestClock{time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
				log := logrus.New()
				log.SetOutput(io.Discard)
				adapter := NewMongoAdapter(db.Collection("users"), db.Collection("pending_friend_requests"), clock, log)
				viewer := id.NewID()
				paused := make([]id.ID, pausedCount)
				for n := range paused {
					paused[n] = id.NewID()
				}
				ids := make([]id.ID, MaxFriendPageSize)
				documents := make([]any, len(ids))
				for n := range ids {
					ids[n] = id.NewID()
					documents[n] = User{
						ID: ids[n], Auth: Auth{Username: fmt.Sprintf("peer-%02d", n), Password: "private"},
						Status: strings.Repeat("s", 1024), PausedUsers: paused,
						Location: &Location{Latitude: 52, Longitude: 21, LastUpdate: clock.now},
					}
				}
				if _, err := adapter.coll.InsertMany(ctx, documents); err != nil {
					b.Fatal(err)
				}
				projection := bson.M{
					"auth.username": 1, locationField: 1, "status": 1, pausedUsersField: 1,
				}
				switch mode {
				case "accepted":
					projection = friendPeerProjection(viewer)
				case "names":
					projection = userNameProjection()
				}
				bytes := projectedListBSONBytes(ctx, b, adapter, ids, projection)
				b.ReportAllocs()
				for b.Loop() {
					var err error
					switch mode {
					case "legacy":
						_, err = adapter.GetUsers(ctx, ids)
					case "accepted":
						_, err = readProjectedUsers[FriendPeer](ctx, adapter, ids, friendPeerProjection(viewer))
					case "names":
						_, err = adapter.GetUserNames(ctx, ids)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(bytes), "returned_BSON_B")
			})
		}
	}
}

func projectedListBSONBytes(
	ctx context.Context, b *testing.B, adapter *mongoUserAdapter, ids []id.ID, projection bson.M,
) int {
	b.Helper()
	cursor, err := adapter.coll.Find(ctx, bson.M{idField: bson.M{"$in": ids}}, options.Find().SetProjection(projection))
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if closeErr := cursor.Close(ctx); closeErr != nil {
			b.Error(closeErr)
		}
	}()
	bytes := 0
	for cursor.Next(ctx) {
		bytes += len(cursor.Current)
	}
	if err := cursor.Err(); err != nil {
		b.Fatal(err)
	}
	return bytes
}
