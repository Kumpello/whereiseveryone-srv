package users

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"whereiseveryone/pkg/id"
)

// FriendListState selects one independently paginated relationship list.
type FriendListState string

// Relationship list states and hard capacity limits.
const (
	FriendListAccepted FriendListState = "accepted"
	FriendListIncoming FriendListState = "pending_incoming"
	FriendListOutgoing FriendListState = "pending_outgoing"
	// MaxFriendPageSize bounds both peer reads and each API response.
	MaxFriendPageSize = 50
	// MaxFriends is the maximum number of accepted friends for either user.
	MaxFriends = 2048
	// MaxIncomingRequests and MaxOutgoingRequests bound each pending list.
	MaxIncomingRequests = 256
	MaxOutgoingRequests = 256
)

// FriendPageQuery uses peer IDs for stable keyset traversal of unchanged data.
type FriendPageQuery struct {
	State FriendListState
	After id.ID
	Limit int
}

// FriendEntry includes only the viewer's friendship metadata for one peer.
type FriendEntry struct {
	User        User
	FriendSince *time.Time
}

// FriendPage contains a bounded list and the last peer ID when more IDs exist.
type FriendPage struct {
	Entries []FriendEntry
	NextID  id.ID
}

func (m *mongoUserAdapter) GetFriendPage(ctx context.Context, viewer id.ID, query FriendPageQuery) (FriendPage, error) {
	if query.Limit < 1 || query.Limit > MaxFriendPageSize {
		return FriendPage{}, errors.New("invalid friend page size")
	}
	var ids []id.ID
	var metadata User
	switch query.State {
	case FriendListAccepted:
		var err error
		metadata, err = m.acceptedFriendPage(ctx, viewer, query)
		if err != nil {
			return FriendPage{}, err
		}
		ids = metadata.SubscribedUsers
	case FriendListIncoming, FriendListOutgoing:
		var err error
		ids, err = m.pendingFriendPageIDs(ctx, viewer, query)
		if err != nil {
			return FriendPage{}, err
		}
	default:
		return FriendPage{}, errors.New("invalid friend list state")
	}
	page := FriendPage{Entries: make([]FriendEntry, 0, min(len(ids), query.Limit))}
	if len(ids) > query.Limit {
		ids = ids[:query.Limit]
		page.NextID = ids[len(ids)-1]
	}
	peers, err := m.GetUsers(ctx, ids)
	if err != nil {
		return FriendPage{}, fmt.Errorf("read friend page peers: %w", err)
	}
	byID := make(map[id.ID]User, len(peers))
	for _, peer := range peers {
		byID[peer.ID] = peer
	}
	for _, peerID := range ids {
		if peer, exists := byID[peerID]; exists {
			page.Entries = append(page.Entries, FriendEntry{User: peer, FriendSince: metadata.FriendSinceFor(peerID)})
		}
	}
	return page, nil
}

//nolint:goconst // Literal BSON field and operator names keep query expressions readable.
func (m *mongoUserAdapter) acceptedFriendPage(ctx context.Context, viewer id.ID, query FriendPageQuery) (User, error) {
	// MongoDB slices the sorted ID list before sending metadata to the server.
	pageIDs := bson.M{"$slice": bson.A{bson.M{"$sortArray": bson.M{
		"input": bson.M{"$filter": bson.M{
			"input": bson.M{"$ifNull": bson.A{"$subscribed_users", bson.A{}}},
			"as":    "peer", "cond": bson.M{"$gt": bson.A{"$$peer", query.After}},
		}}, "sortBy": 1,
	}}, query.Limit + 1}}
	pageKeys := bson.M{"$map": bson.M{
		"input": "$subscribed_users", "as": "peer", "in": bson.M{"$toString": "$$peer"},
	}}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: withUserId(viewer)}},
		{{Key: "$project", Value: bson.M{"subscribed_users": pageIDs, "friend_since": 1}}},
		{{Key: "$project", Value: bson.M{
			"subscribed_users": 1,
			"friend_since": bson.M{"$arrayToObject": bson.M{"$filter": bson.M{
				"input": bson.M{"$objectToArray": bson.M{"$ifNull": bson.A{"$friend_since", bson.M{}}}},
				"as":    "entry", "cond": bson.M{"$in": bson.A{"$$entry.k", pageKeys}},
			}}},
		}}},
	}
	cursor, err := m.coll.Aggregate(ctx, pipeline)
	if err != nil {
		return User{}, fmt.Errorf("query accepted friend page: %w", err)
	}
	defer func() {
		if closeErr := cursor.Close(ctx); closeErr != nil {
			m.logger.Errorf("close friend page: %v", closeErr)
		}
	}()
	if !cursor.Next(ctx) {
		if err := cursor.Err(); err != nil {
			return User{}, fmt.Errorf("read accepted friend page: %w", err)
		}
		return User{}, ErrUserNotExists
	}
	var metadata User
	if err := cursor.Decode(&metadata); err != nil {
		return User{}, fmt.Errorf("decode accepted friend page: %w", err)
	}
	return metadata, nil
}

//nolint:goconst // Literal BSON field and operator names keep query expressions readable.
func (m *mongoUserAdapter) pendingFriendPageIDs(
	ctx context.Context, viewer id.ID, query FriendPageQuery,
) ([]id.ID, error) {
	owner, peer := "to", "from"
	if query.State == FriendListOutgoing {
		owner, peer = peer, owner
	}
	filter := bson.M{owner: viewer, peer: bson.M{"$gt": query.After}}
	opts := options.Find().SetSort(bson.D{{Key: peer, Value: 1}}).
		SetLimit(int64(query.Limit + 1)).SetProjection(bson.M{idField: 0, peer: 1})
	cursor, err := m.requests.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("query pending friend page: %w", err)
	}
	defer func() {
		if closeErr := cursor.Close(ctx); closeErr != nil {
			m.logger.Errorf("close friend page: %v", closeErr)
		}
	}()
	ids := make([]id.ID, 0, query.Limit+1)
	for cursor.Next(ctx) {
		var request PendingFriendRequest
		if err := cursor.Decode(&request); err != nil {
			return nil, fmt.Errorf("decode pending friend page: %w", err)
		}
		peerID := request.From
		if query.State == FriendListOutgoing {
			peerID = request.To
		}
		ids = append(ids, peerID)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("read pending friend page: %w", err)
	}
	return ids, nil
}
