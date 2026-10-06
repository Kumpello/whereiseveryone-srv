package users

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"whereiseveryone/pkg/id"
)

// UserName is the identity-only view used by pending and paused-sharing lists.
type UserName struct {
	ID       id.ID  `bson:"_id"` //nolint:tagliatelle // MongoDB document identifier.
	Username string `bson:"username"`
}

// FriendPeer contains accepted-list fields and a viewer-specific visibility decision.
// It is a projected view, not a complete User.
type FriendPeer struct {
	UserName        `bson:",inline"`
	Status          string    `bson:"status"`
	Location        *Location `bson:"location"`
	LocationVisible bool      `bson:"location_visible"`
}

func userNameProjection() bson.M {
	return bson.M{idField: 1, "username": "$auth.username"}
}

//nolint:goconst // Literal BSON field and operator names keep query expressions readable.
func friendPeerProjection(viewer id.ID) bson.M {
	projection := userNameProjection()
	projection["status"] = 1
	projection[locationField] = 1
	// Match the driver's legacy hex-string ID decoding. Unconvertible entries
	// conservatively hide location rather than bypassing a possible pause.
	pausedIDs := bson.M{"$map": bson.M{
		"input": bson.M{"$ifNull": bson.A{"$" + pausedUsersField, bson.A{}}},
		"as":    "peer", "in": bson.M{"$convert": bson.M{
			"input": "$$peer", "to": "objectId", "onError": viewer, "onNull": id.ID{},
		}},
	}}
	projection["location_visible"] = bson.M{"$not": bson.A{bson.M{"$in": bson.A{
		viewer, pausedIDs,
	}}}}
	return projection
}

// GetUserNames reads only IDs and usernames, omitting profile and relationship data.
func (m *mongoUserAdapter) GetUserNames(ctx context.Context, ids []id.ID) ([]UserName, error) {
	return readProjectedUsers[UserName](ctx, m, ids, userNameProjection())
}

// GetPausedUsers reads only the viewer's paused IDs and the matching usernames.
func (m *mongoUserAdapter) GetPausedUsers(ctx context.Context, viewer id.ID) ([]UserName, error) {
	var metadata struct {
		IDs []id.ID `bson:"paused_users"`
	}
	opts := options.FindOne().SetProjection(bson.M{idField: 0, pausedUsersField: 1})
	if err := m.coll.FindOne(ctx, withUserId(viewer), opts).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("read paused user IDs: %w", err)
	}
	names, err := m.GetUserNames(ctx, metadata.IDs)
	if err != nil {
		return nil, fmt.Errorf("read paused usernames: %w", err)
	}
	return names, nil
}

func readProjectedUsers[T any](
	ctx context.Context, m *mongoUserAdapter, ids []id.ID, projection bson.M,
) ([]T, error) {
	result := make([]T, 0, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	filter := bson.M{idField: bson.M{"$in": ids}}
	cursor, err := m.coll.Find(ctx, filter, options.Find().SetProjection(projection))
	if err != nil {
		return nil, fmt.Errorf("query list users: %w", err)
	}
	defer func() {
		if closeErr := cursor.Close(ctx); closeErr != nil {
			m.logger.Errorf("close list users: %v", closeErr)
		}
	}()
	for cursor.Next(ctx) {
		var user T
		if err := cursor.Decode(&user); err != nil {
			return nil, fmt.Errorf("decode list user: %w", err)
		}
		result = append(result, user)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("read list users: %w", err)
	}
	return result, nil
}
