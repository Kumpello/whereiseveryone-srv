package users

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"go.mongodb.org/mongo-driver/v2/bson"

	"whereiseveryone/pkg/id"
)

func newRelationshipTestStore(t *testing.T) (context.Context, *mongoUserAdapter) {
	t.Helper()
	ctx, db := newMongoTestDatabase(t)
	log := logrus.New()
	log.SetOutput(io.Discard)
	clock := authTestClock{time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	adapter := NewMongoAdapter(db.Collection("users"), db.Collection("pending_friend_requests"), clock, log)
	if err := adapter.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	return ctx, adapter
}

func seedRelationshipUser(ctx context.Context, t *testing.T, adapter *mongoUserAdapter, friendCount int) User {
	t.Helper()
	u := User{Auth: Auth{Username: id.NewID().Hex()}}
	for range friendCount {
		u.SubscribedUsers = append(u.SubscribedUsers, id.NewID())
	}
	u, err := adapter.NewUser(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestMongoFriendPages(t *testing.T) {
	ctx, adapter := newRelationshipTestStore(t)
	viewer := seedRelationshipUser(ctx, t, adapter, 0)
	const count = 123
	ids := make([]id.ID, count)
	docs, requests := make([]any, 0, count), make([]any, 0, count*2)
	since := make(map[string]time.Time)
	for n := range count {
		peerID := id.NewID()
		ids[n] = peerID
		since[peerID.Hex()] = adapter.timer.Now()
		docs = append(docs, User{ID: peerID, Auth: Auth{Username: fmt.Sprint(n), Password: "private"},
			Status: "private status", Location: &Location{LastUpdate: adapter.timer.Now()}})
		requests = append(requests, PendingFriendRequest{ID: id.NewID(), From: peerID, To: viewer.ID},
			PendingFriendRequest{ID: id.NewID(), From: viewer.ID, To: peerID})
	}
	if _, err := adapter.coll.InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.requests.InsertMany(ctx, requests); err != nil {
		t.Fatal(err)
	}
	// Reverse storage order to prove traversal orders peer IDs, not array positions.
	reversed := make([]id.ID, count)
	for n := range ids {
		reversed[count-1-n] = ids[n]
	}
	if _, err := adapter.coll.UpdateOne(ctx, withUserId(viewer.ID), bson.M{"$set": bson.M{
		"subscribed_users": reversed, "friend_since": since,
	}}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []FriendListState{FriendListAccepted, FriendListIncoming, FriendListOutgoing} {
		query := FriendPageQuery{State: state, Limit: MaxFriendPageSize}
		seen := 0
		for {
			page, err := adapter.GetFriendPage(ctx, viewer.ID, query)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Entries) > MaxFriendPageSize {
				t.Fatal("unbounded peer read")
			}
			for _, entry := range page.Entries {
				if entry.Peer.ID != ids[seen] {
					t.Fatal("page order changed")
				}
				if state == FriendListAccepted && (entry.FriendSince == nil || !entry.FriendSince.Equal(since[entry.Peer.ID.Hex()])) {
					t.Fatal("page lost friendship metadata")
				}
				if state != FriendListAccepted && entry.FriendSince != nil {
					t.Fatal("pending metadata leaked")
				}
				if state != FriendListAccepted && (entry.Peer.Status != "" || entry.Peer.Location != nil || entry.Peer.LocationVisible) {
					t.Fatal("pending peer projection fetched private profile fields")
				}
				seen++
			}
			if page.NextID.IsZero() {
				break
			}
			if page.NextID.Hex() <= query.After.Hex() {
				t.Fatal("cursor did not advance")
			}
			query.After = page.NextID
		}
		if seen != count {
			t.Fatalf("%s traversal returned %d entries", state, seen)
		}
		final, err := adapter.GetFriendPage(ctx, viewer.ID, FriendPageQuery{State: state, After: ids[count-1], Limit: 50})
		if err != nil || len(final.Entries) != 0 || !final.NextID.IsZero() {
			t.Fatalf("empty final page: %+v, %v", final, err)
		}
	}
	for _, limit := range []int{0, 51} {
		if _, err := adapter.GetFriendPage(ctx, viewer.ID, FriendPageQuery{State: FriendListAccepted, Limit: limit}); err == nil {
			t.Fatal("invalid internal page limit accepted")
		}
	}
	metadata, err := adapter.acceptedFriendPage(ctx, viewer.ID, FriendPageQuery{State: FriendListAccepted, Limit: 10})
	if err != nil || len(metadata.SubscribedUsers) != 11 || len(metadata.FriendSince) != 11 {
		t.Fatalf("metadata transfer must be bounded by limit+1: %+v, %v", metadata, err)
	}
}

func TestMongoPendingLimitsUnderConcurrency(t *testing.T) {
	for _, incoming := range []bool{false, true} {
		t.Run(fmt.Sprintf("incoming=%t", incoming), func(t *testing.T) {
			ctx, adapter := newRelationshipTestStore(t)
			owner := seedRelationshipUser(ctx, t, adapter, 0)
			requests := make([]any, 0, 255)
			for range 255 {
				from, to := owner.ID, id.NewID()
				if incoming {
					from, to = to, from
				}
				requests = append(requests, PendingFriendRequest{ID: id.NewID(), From: from, To: to})
			}
			if _, err := adapter.requests.InsertMany(ctx, requests); err != nil {
				t.Fatal(err)
			}
			const attempts = 8
			peers := make([]User, attempts)
			for n := range peers {
				peers[n] = seedRelationshipUser(ctx, t, adapter, 0)
			}
			errs := make(chan error, attempts)
			var workers sync.WaitGroup
			for _, peer := range peers {
				workers.Go(func() {
					from, to := owner.ID, peer.ID
					if incoming {
						from, to = to, from
					}
					errs <- adapter.SendFriendRequest(ctx, from, to)
				})
			}
			workers.Wait()
			close(errs)
			wantErr, field := ErrOutgoingRequestsLimit, "from"
			if incoming {
				wantErr, field = ErrIncomingRequestsLimit, "to"
			}
			successes := 0
			for err := range errs {
				if err == nil {
					successes++
				} else if !errors.Is(err, wantErr) {
					t.Fatal(err)
				}
			}
			count, err := adapter.requests.CountDocuments(ctx, bson.M{field: owner.ID})
			if err != nil || count != 256 || successes != 1 {
				t.Fatalf("count=%d successes=%d error=%v", count, successes, err)
			}
			var existing PendingFriendRequest
			if err := adapter.requests.FindOne(ctx, bson.M{field: owner.ID}).Decode(&existing); err != nil {
				t.Fatal(err)
			}
			// Use a real peer for the duplicate and for releasing capacity.
			for _, peer := range peers {
				from, to := owner.ID, peer.ID
				if incoming {
					from, to = to, from
				}
				if err := adapter.requests.FindOne(ctx, bson.M{"from": from, "to": to}).Err(); err != nil {
					continue
				}
				if err := adapter.SendFriendRequest(ctx, from, to); err != nil {
					t.Fatalf("duplicate at capacity: %v", err)
				}
				if err := adapter.RejectFriendRequest(ctx, to, from); err != nil {
					t.Fatal(err)
				}
				if err := adapter.SendFriendRequest(ctx, from, to); err != nil {
					t.Fatalf("released pending slot: %v", err)
				}
				break
			}
		})
	}
}

func TestMongoAcceptedLimitUnderConcurrency(t *testing.T) {
	ctx, adapter := newRelationshipTestStore(t)
	viewer := seedRelationshipUser(ctx, t, adapter, MaxFriends-1)
	const attempts = 8
	peers := make([]User, attempts)
	requests := make([]any, 0, attempts)
	for n := range peers {
		peers[n] = seedRelationshipUser(ctx, t, adapter, 0)
		requests = append(requests, PendingFriendRequest{ID: id.NewID(), From: peers[n].ID, To: viewer.ID})
	}
	if _, err := adapter.requests.InsertMany(ctx, requests); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, attempts)
	var workers sync.WaitGroup
	for _, peer := range peers {
		workers.Go(func() { errs <- adapter.AcceptFriendRequest(ctx, viewer.ID, peer.ID) })
	}
	workers.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrFriendsLimit) {
			t.Fatal(err)
		}
	}
	stored, err := adapter.GetUser(ctx, viewer.ID)
	if err != nil || len(stored.SubscribedUsers) != MaxFriends || successes != 1 {
		t.Fatalf("accepted=%d successes=%d error=%v", len(stored.SubscribedUsers), successes, err)
	}
	count, err := adapter.requests.CountDocuments(ctx, bson.M{"to": viewer.ID})
	if err != nil || count != attempts-1 {
		t.Fatal("capacity rejection consumed pending requests")
	}
	for _, peer := range peers {
		other, err := adapter.GetUser(ctx, peer.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.SubscribeUser(peer.ID) != other.SubscribeUser(viewer.ID) {
			t.Fatal("one-sided friendship")
		}
		if stored.SubscribeUser(peer.ID) {
			if err := adapter.UnfriendUser(ctx, viewer.ID, peer.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, peer := range peers {
		if stored.SubscribeUser(peer.ID) {
			continue
		}
		if err := adapter.AcceptFriendRequest(ctx, viewer.ID, peer.ID); err != nil {
			t.Fatalf("released accepted slot: %v", err)
		}
		break
	}
}

func TestMongoFullCounterpartyDoesNotConsumeRequest(t *testing.T) {
	ctx, adapter := newRelationshipTestStore(t)
	viewer := seedRelationshipUser(ctx, t, adapter, 0)
	full := seedRelationshipUser(ctx, t, adapter, MaxFriends)
	request := PendingFriendRequest{ID: id.NewID(), From: full.ID, To: viewer.ID}
	if _, err := adapter.requests.InsertOne(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := adapter.AcceptFriendRequest(ctx, viewer.ID, full.ID); !errors.Is(err, ErrFriendsLimit) {
		t.Fatalf("error=%v", err)
	}
	if err := adapter.requests.FindOne(ctx, bson.M{idField: request.ID}).Err(); err != nil {
		t.Fatal("failed accept lost request")
	}
	stored, err := adapter.GetUser(ctx, viewer.ID)
	if err != nil || len(stored.SubscribedUsers) != 0 {
		t.Fatal("failed accept partially changed friendship")
	}
	if err := adapter.SendFriendRequest(ctx, viewer.ID, full.ID); !errors.Is(err, ErrFriendsLimit) {
		t.Fatalf("full target send error=%v", err)
	}
	if err := adapter.AddFriend(ctx, full.ID, viewer.ID); !errors.Is(err, ErrFriendsLimit) {
		t.Fatalf("direct add bypassed limit: %v", err)
	}
}

func TestMongoAcceptanceRollsBackAfterPeerWriteFails(t *testing.T) {
	ctx, adapter := newRelationshipTestStore(t)
	viewer := seedRelationshipUser(ctx, t, adapter, 0)
	peer := seedRelationshipUser(ctx, t, adapter, 0)
	if _, err := adapter.coll.UpdateOne(ctx, withUserId(peer.ID), bson.M{
		"$set": bson.M{"subscribed_users": bson.A{}},
	}); err != nil {
		t.Fatal(err)
	}
	request := PendingFriendRequest{ID: id.NewID(), From: peer.ID, To: viewer.ID}
	if _, err := adapter.requests.InsertOne(ctx, request); err != nil {
		t.Fatal(err)
	}
	// Allow the revision write but reject the peer's later friendship write.
	validator := bson.M{"$or": bson.A{
		bson.M{idField: bson.M{"$ne": peer.ID}},
		bson.M{"subscribed_users": bson.M{"$size": 0}},
	}}
	if err := adapter.coll.Database().RunCommand(ctx, bson.D{
		{Key: "collMod", Value: adapter.coll.Name()}, {Key: "validator", Value: validator},
	}).Err(); err != nil {
		t.Fatal(err)
	}
	if err := adapter.AcceptFriendRequest(ctx, viewer.ID, peer.ID); err == nil {
		t.Fatal("expected peer validation failure")
	}
	for _, userID := range []id.ID{viewer.ID, peer.ID} {
		stored, err := adapter.GetUser(ctx, userID)
		if err != nil || len(stored.SubscribedUsers) != 0 || len(stored.FriendSince) != 0 {
			t.Fatal("failed second write left a partial friendship")
		}
		if err := adapter.coll.FindOne(ctx, bson.M{idField: userID, "relationships_version": bson.M{"$exists": true}}).Err(); !errors.Is(err, ErrUserNotExists) {
			t.Fatalf("aborted revision remained: %v", err)
		}
	}
	if err := adapter.requests.FindOne(ctx, bson.M{idField: request.ID}).Err(); err != nil {
		t.Fatal("failed transaction consumed the pending request")
	}
}
