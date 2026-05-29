package users

import (
	"context"
	"fmt"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/logger"
	"whereiseveryone/pkg/pointers"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type PendingFriendRequest struct {
	ID   id.ID `bson:"_id"` //nolint:tagliatelle // mongo-id
	From id.ID `bson:"from"`
	To   id.ID `bson:"to"`
}

type pendingFriendRequestAdapter interface {
	EnsureIndexes(ctx context.Context) error
	GetPendingIncomingFriendRequestUserIDs(ctx context.Context, user id.ID) ([]id.ID, error)
	GetPendingOutgoingFriendRequestUserIDs(ctx context.Context, user id.ID) ([]id.ID, error)
	SendFriendRequest(ctx context.Context, from id.ID, to id.ID) error
	DeleteFriendRequest(ctx context.Context, user id.ID, requester id.ID) error
}

type mongoPendingFriendRequestAdapter struct {
	coll   *mongo.Collection
	logger logger.Logger
}

func (m mongoPendingFriendRequestAdapter) EnsureIndexes(ctx context.Context) error {
	unique := options.IndexOptions{
		Unique: pointers.Pointer(true),
	}

	uniqueRequestIdx := mongo.IndexModel{
		Keys: bson.D{
			{Key: "from", Value: 1},
			{Key: "to", Value: 1},
		},
		Options: &unique,
	}
	incomingRequestsIdx := mongo.IndexModel{
		Keys: bson.D{
			{Key: "to", Value: 1},
		},
	}

	_, err := m.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		uniqueRequestIdx,
		incomingRequestsIdx,
	})
	if err != nil {
		return fmt.Errorf("create pending friend request indexes: %w", err)
	}

	m.logger.Infof("Created indexes on pending friend request fields `from,to` and `to`")

	return nil
}

func (m mongoPendingFriendRequestAdapter) GetPendingIncomingFriendRequestUserIDs(
	ctx context.Context,
	user id.ID,
) ([]id.ID, error) {
	filter := bson.M{
		"to": user,
	}

	c, err := m.coll.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("find incoming pending friend requests: %w", err)
	}

	var requests []PendingFriendRequest
	if err := c.All(ctx, &requests); err != nil {
		return nil, fmt.Errorf("decode incoming pending friend requests: %w", err)
	}

	userIDs := make([]id.ID, 0, len(requests))
	for _, request := range requests {
		userIDs = append(userIDs, request.From)
	}

	return userIDs, nil
}

func (m mongoPendingFriendRequestAdapter) GetPendingOutgoingFriendRequestUserIDs(
	ctx context.Context,
	user id.ID,
) ([]id.ID, error) {
	filter := bson.M{
		"from": user,
	}

	c, err := m.coll.Find(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("find outgoing pending friend requests: %w", err)
	}

	var requests []PendingFriendRequest
	if err := c.All(ctx, &requests); err != nil {
		return nil, fmt.Errorf("decode outgoing pending friend requests: %w", err)
	}

	userIDs := make([]id.ID, 0, len(requests))
	for _, request := range requests {
		userIDs = append(userIDs, request.To)
	}

	return userIDs, nil
}

func (m mongoPendingFriendRequestAdapter) SendFriendRequest(
	ctx context.Context,
	from id.ID,
	to id.ID,
) error {
	filter := bson.M{
		"from": from,
		"to":   to,
	}
	update := bson.M{
		"$setOnInsert": PendingFriendRequest{
			ID:   id.NewID(),
			From: from,
			To:   to,
		},
	}
	opts := options.Update().SetUpsert(true)

	_, err := m.coll.UpdateOne(ctx, filter, update, opts)
	if err != nil {
		return fmt.Errorf("upsert pending friend request: %w", err)
	}

	return nil
}

func (m mongoPendingFriendRequestAdapter) DeleteFriendRequest(
	ctx context.Context,
	user id.ID,
	requester id.ID,
) error {
	filter := bson.M{
		"from": requester,
		"to":   user,
	}

	_, err := m.coll.DeleteOne(ctx, filter)
	if err != nil {
		return fmt.Errorf("remove pending friend request: %w", err)
	}

	return nil
}

var _ pendingFriendRequestAdapter = (*mongoPendingFriendRequestAdapter)(nil)
