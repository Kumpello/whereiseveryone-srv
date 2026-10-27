package users

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"whereiseveryone/pkg/id"
)

func TestMongoPointLimitOwnershipAndImmutableMetadata(t *testing.T) {
	ctx, adapter := newRelationshipTestStore(t)
	owner := seedRelationshipUser(ctx, t, adapter, 0)
	other := seedRelationshipUser(ctx, t, adapter, 0)
	points, readErr := adapter.GetPointsOfInterest(ctx, owner.ID)
	if readErr != nil || points == nil || len(points) != 0 {
		t.Fatalf("new owner points=%+v err=%v", points, readErr)
	}
	const attempts = 24
	results := make(chan error, attempts)
	var workers sync.WaitGroup
	for n := range attempts {
		workers.Go(func() {
			_, createErr := adapter.CreatePointOfInterest(ctx, owner.ID, PointOfInterest{
				ID: id.NewID(), Name: fmt.Sprintf("Point %d", n), Description: "A shared place",
				Latitude: 52, Longitude: 21, CreatedAt: time.Unix(1, 0),
			})
			results <- createErr
		})
	}
	workers.Wait()
	close(results)
	created := 0
	for result := range results {
		if result == nil {
			created++
		} else if !errors.Is(result, ErrPointsOfInterestLimit) {
			t.Fatal(result)
		}
	}
	points, readErr = adapter.GetPointsOfInterest(ctx, owner.ID)
	if readErr != nil || created != MaxPointsOfInterest || len(points) != MaxPointsOfInterest {
		t.Fatalf("created=%d stored=%d err=%v", created, len(points), readErr)
	}
	ids := make(map[id.ID]bool)
	for _, point := range points {
		if point.ID.IsZero() || ids[point.ID] || !point.CreatedAt.Equal(adapter.timer.Now()) {
			t.Fatalf("invalid server metadata: %+v", point)
		}
		ids[point.ID] = true
	}
	original := points[0]
	replacement := PointOfInterest{Name: "Renamed", Description: "Updated", Latitude: 0, Longitude: 0,
		ID: id.NewID(), CreatedAt: adapter.timer.Now().Add(time.Hour)}
	if _, err := adapter.UpdatePointOfInterest(ctx, other.ID, original.ID, replacement); !errors.Is(err, ErrPointOfInterestNotFound) {
		t.Fatalf("another owner edited a point: %v", err)
	}
	if err := adapter.DeletePointOfInterest(ctx, other.ID, original.ID); !errors.Is(err, ErrPointOfInterestNotFound) {
		t.Fatalf("another owner deleted a point: %v", err)
	}
	updated, updateErr := adapter.UpdatePointOfInterest(ctx, owner.ID, original.ID, replacement)
	if updateErr != nil || updated.ID != original.ID || !updated.CreatedAt.Equal(original.CreatedAt) ||
		updated.Name != replacement.Name || updated.Description != replacement.Description || updated.Latitude != 0 || updated.Longitude != 0 {
		t.Fatalf("updated=%+v err=%v", updated, updateErr)
	}
	if err := adapter.DeletePointOfInterest(ctx, owner.ID, original.ID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DeletePointOfInterest(ctx, owner.ID, original.ID); !errors.Is(err, ErrPointOfInterestNotFound) {
		t.Fatalf("duplicate delete=%v", err)
	}
	if _, err := adapter.CreatePointOfInterest(ctx, owner.ID, replacement); err != nil {
		t.Fatalf("deletion did not free a point slot: %v", err)
	}
	points, readErr = adapter.GetPointsOfInterest(ctx, owner.ID)
	if readErr != nil || len(points) != MaxPointsOfInterest {
		t.Fatalf("points after replacement=%+v err=%v", points, readErr)
	}
}

func TestMongoFriendPointsAreIndependentOfPausedLiveLocation(t *testing.T) {
	ctx, adapter := newRelationshipTestStore(t)
	viewer := seedRelationshipUser(ctx, t, adapter, 0)
	creator, createErr := adapter.NewUser(ctx, User{Auth: Auth{Username: "creator"}, PausedUsers: []id.ID{}})
	if createErr != nil {
		t.Fatal(createErr)
	}
	pending := seedRelationshipUser(ctx, t, adapter, 0)
	for _, owner := range []id.ID{creator.ID, pending.ID} {
		if _, err := adapter.CreatePointOfInterest(ctx, owner, PointOfInterest{Name: "Meeting place", Latitude: 52, Longitude: 21}); err != nil {
			t.Fatal(err)
		}
	}
	if err := adapter.SendFriendRequest(ctx, creator.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.AcceptFriendRequest(ctx, viewer.ID, creator.ID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SendFriendRequest(ctx, pending.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	if err := adapter.StopSharing(ctx, creator.ID, viewer.ID); err != nil {
		t.Fatal(err)
	}
	page, err := adapter.GetFriendPage(ctx, viewer.ID, FriendPageQuery{State: FriendListAccepted, Limit: 50})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Peer.LocationVisible || len(page.Entries[0].Peer.PointsOfInterest) != 1 {
		t.Fatalf("accepted paused friend page=%+v err=%v", page, err)
	}
	page, err = adapter.GetFriendPage(ctx, viewer.ID, FriendPageQuery{State: FriendListIncoming, Limit: 50})
	if err != nil || len(page.Entries) != 1 || len(page.Entries[0].Peer.PointsOfInterest) != 0 {
		t.Fatalf("pending friend leaked points: %+v err=%v", page, err)
	}
	if unfriendErr := adapter.UnfriendUser(ctx, viewer.ID, creator.ID); unfriendErr != nil {
		t.Fatal(unfriendErr)
	}
	page, err = adapter.GetFriendPage(ctx, viewer.ID, FriendPageQuery{State: FriendListAccepted, Limit: 50})
	if err != nil || len(page.Entries) != 0 {
		t.Fatalf("former friend still visible: %+v err=%v", page, err)
	}
}
