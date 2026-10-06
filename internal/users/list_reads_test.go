package users

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"whereiseveryone/pkg/id"
)

func TestMongoFriendPeerVisibilityProjection(t *testing.T) {
	ctx, adapter := newRelationshipTestStore(t)
	viewer := seedRelationshipUser(ctx, t, adapter, 0)
	other := id.NewID()
	location := &Location{Latitude: 52, Longitude: 21, LastUpdate: adapter.timer.Now()}
	for _, tc := range []struct {
		name    string
		paused  any
		omit    bool
		visible bool
	}{
		{"missing", nil, true, true},
		{"null", nil, false, true},
		{"empty", []id.ID{}, false, true},
		{"other viewer", []id.ID{other}, false, true},
		{"viewer first", []id.ID{viewer.ID, other}, false, false},
		{"viewer last", []id.ID{other, viewer.ID}, false, false},
		{"viewer repeated", []id.ID{viewer.ID, viewer.ID}, false, false},
		{"legacy viewer hex", bson.A{viewer.ID.Hex()}, false, false},
		{"legacy viewer uppercase hex", bson.A{strings.ToUpper(viewer.ID.Hex())}, false, false},
		{"legacy other hex", bson.A{other.Hex()}, false, true},
		{"null entry", bson.A{nil}, false, true},
		{"invalid element fails closed", bson.A{true}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peerID := id.NewID()
			doc := bson.M{
				idField: peerID, "auth": bson.M{"username": tc.name, "password": "private", "token": "private"},
				"status": "friend status", locationField: location,
				"subscribed_users": []id.ID{other}, "friend_since": bson.M{other.Hex(): adapter.timer.Now()},
			}
			if !tc.omit {
				doc[pausedUsersField] = tc.paused
			}
			if _, err := adapter.coll.InsertOne(ctx, doc); err != nil {
				t.Fatal(err)
			}
			if err := adapter.AddFriend(ctx, viewer.ID, peerID); err != nil {
				t.Fatal(err)
			}
			page, err := adapter.GetFriendPage(ctx, viewer.ID, FriendPageQuery{State: FriendListAccepted, Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			var got *FriendPeer
			for i := range page.Entries {
				if page.Entries[i].Peer.ID == peerID {
					got = &page.Entries[i].Peer
				}
			}
			if got == nil || got.Username != tc.name || got.Status != "friend status" ||
				!reflect.DeepEqual(got.Location, location) || got.LocationVisible != tc.visible {
				t.Fatalf("projected peer=%+v, want visible=%t", got, tc.visible)
			}
			raw := adapter.coll.FindOne(ctx, withUserId(peerID), options.FindOne().SetProjection(friendPeerProjection(viewer.ID)))
			var projected bson.M
			if err := raw.Decode(&projected); err != nil {
				t.Fatal(err)
			}
			if len(projected) != 5 || projected["location_visible"] != tc.visible {
				t.Fatalf("unexpected accepted projection fields: %+v", projected)
			}
			for _, forbidden := range []string{"auth", pausedUsersField, "subscribed_users", "friend_since"} {
				if _, ok := projected[forbidden]; ok {
					t.Fatalf("accepted peer fetched %s", forbidden)
				}
			}
		})
	}
}

func TestMongoIdentityOnlyListsIgnoreUnneededFields(t *testing.T) {
	ctx, adapter := newRelationshipTestStore(t)
	viewer := seedRelationshipUser(ctx, t, adapter, 0)
	peerID := id.NewID()
	// Invalid types in fields not used by these lists prove that they are not decoded.
	if _, err := adapter.coll.InsertOne(ctx, bson.M{
		idField: peerID, "auth": bson.M{"username": "peer", "password": bson.M{"not_a_string": true}},
		"status": bson.M{"not_a_string": true}, locationField: "not_a_location",
		pausedUsersField: "not_an_array", "friend_since": "not_a_map", "subscribed_users": "not_an_array",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.requests.InsertMany(ctx, []any{
		PendingFriendRequest{ID: id.NewID(), From: peerID, To: viewer.ID},
		PendingFriendRequest{ID: id.NewID(), From: viewer.ID, To: peerID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.coll.UpdateOne(ctx, withUserId(viewer.ID), bson.M{"$set": bson.M{
		pausedUsersField: []id.ID{peerID, peerID, id.NewID()}, "status": bson.M{"not_a_string": true},
		locationField: "not_a_location", "subscribed_users": "not_an_array",
	}}); err != nil {
		t.Fatal(err)
	}
	want := UserName{ID: peerID, Username: "peer"}
	names, err := adapter.GetPausedUsers(ctx, viewer.ID)
	if err != nil || !reflect.DeepEqual(names, []UserName{want}) {
		t.Fatalf("paused names=%+v, error=%v", names, err)
	}
	for _, state := range []FriendListState{FriendListIncoming, FriendListOutgoing} {
		page, pageErr := adapter.GetFriendPage(ctx, viewer.ID, FriendPageQuery{State: state, Limit: 50})
		if pageErr != nil || len(page.Entries) != 1 || page.Entries[0].Peer != (FriendPeer{UserName: want}) {
			t.Fatalf("%s page=%+v, error=%v", state, page, pageErr)
		}
	}
	var raw bson.M
	if err := adapter.coll.FindOne(ctx, withUserId(peerID),
		options.FindOne().SetProjection(userNameProjection())).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 || raw["username"] != "peer" || raw[idField] != peerID {
		t.Fatalf("identity query returned extra fields: %+v", raw)
	}
	// An invalid non-array pause list must fail the accepted query, never reveal location.
	if _, err := readProjectedUsers[FriendPeer](ctx, adapter, []id.ID{peerID}, friendPeerProjection(viewer.ID)); err == nil {
		t.Fatal("invalid accepted visibility metadata did not fail closed")
	}
}

func TestMongoListReadBoundaries(t *testing.T) {
	ctx, adapter := newRelationshipTestStore(t)
	for _, ids := range [][]id.ID{nil, {}, {id.NewID()}} {
		got, err := adapter.GetUserNames(ctx, ids)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("empty/missing names=%+v, error=%v", got, err)
		}
	}
	for _, paused := range []any{nil, []id.ID{}} {
		viewerID := id.NewID()
		if _, err := adapter.coll.InsertOne(ctx, bson.M{
			idField: viewerID, "auth": bson.M{"username": viewerID.Hex()}, pausedUsersField: paused,
		}); err != nil {
			t.Fatal(err)
		}
		got, err := adapter.GetPausedUsers(ctx, viewerID)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("empty paused names=%+v, error=%v", got, err)
		}
	}
	missing := seedRelationshipUser(ctx, t, adapter, 0)
	peers, err := readProjectedUsers[FriendPeer](ctx, adapter, []id.ID{missing.ID}, friendPeerProjection(id.NewID()))
	if err != nil || len(peers) != 1 || peers[0].Location != nil || !peers[0].LocationVisible {
		t.Fatalf("accepted peer without location: peers=%+v, error=%v", peers, err)
	}
	if _, err := adapter.coll.UpdateOne(ctx, withUserId(missing.ID), bson.M{"$unset": bson.M{pausedUsersField: ""}}); err != nil {
		t.Fatal(err)
	}
	if got, err := adapter.GetPausedUsers(ctx, missing.ID); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("missing pause field: names=%+v, error=%v", got, err)
	}
	if _, err := adapter.GetPausedUsers(ctx, id.NewID()); !errors.Is(err, ErrUserNotExists) {
		t.Fatalf("missing viewer error=%v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := adapter.GetUserNames(canceled, []id.ID{missing.ID}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled name read error=%v", err)
	}
	if _, err := adapter.GetPausedUsers(canceled, missing.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled paused list error=%v", err)
	}
}

func TestMongoListProjectionTransferIsIndependentOfPauseList(t *testing.T) {
	ctx, adapter := newRelationshipTestStore(t)
	viewer, peer := id.NewID(), User{Auth: Auth{Username: "peer"}, Status: strings.Repeat("s", 1024)}
	peer.Location = &Location{Latitude: 52, LastUpdate: adapter.timer.Now()}
	var err error
	peer, err = adapter.NewUser(ctx, peer)
	if err != nil {
		t.Fatal(err)
	}
	lengths := make(map[string]int)
	for _, count := range []int{0, 1000, 10000} {
		paused := make([]id.ID, count)
		for n := range paused {
			paused[n] = id.NewID()
		}
		if _, err := adapter.coll.UpdateOne(ctx, withUserId(peer.ID), bson.M{"$set": bson.M{pausedUsersField: paused}}); err != nil {
			t.Fatal(err)
		}
		for name, projection := range map[string]bson.M{"names": userNameProjection(), "accepted": friendPeerProjection(viewer)} {
			raw, readErr := adapter.coll.FindOne(ctx, withUserId(peer.ID), options.FindOne().SetProjection(projection)).Raw()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if count == 0 {
				lengths[name] = len(raw)
			} else if len(raw) != lengths[name] {
				t.Fatalf("%s returned BSON grew with %d pause IDs: %d vs %d", name, count, len(raw), lengths[name])
			}
			t.Logf("projection=%s pause_ids=%d returned_BSON_bytes=%d", name, count, len(raw))
		}
	}
}
