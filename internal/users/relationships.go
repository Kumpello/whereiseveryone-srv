package users

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"whereiseveryone/pkg/id"
)

// Sentinel errors let the API distinguish permanent capacity conflicts.
var (
	ErrFriendsLimit          = errors.New("friend limit reached")
	ErrIncomingRequestsLimit = errors.New("incoming request limit reached")
	ErrOutgoingRequestsLimit = errors.New("outgoing request limit reached")
	ErrAlreadyFriends        = errors.New("users are already friends")
)

// relationshipTransaction serializes mutations for both accounts, even across
// server instances. Counting alone under snapshot isolation permits write skew.
// Touching both user documents first makes competing operations conflict and retry.
func (m *mongoUserAdapter) relationshipTransaction(
	ctx context.Context, first, second id.ID, mutate func(context.Context) error,
) error {
	if first == second {
		return errors.New("cannot create a relationship with yourself")
	}
	session, err := m.coll.Database().Client().StartSession()
	if err != nil {
		return fmt.Errorf("start relationship session: %w", err)
	}
	defer session.EndSession(ctx)
	if first.Hex() > second.Hex() {
		first, second = second, first
	}
	opts := options.Transaction().SetReadConcern(readconcern.Snapshot()).SetWriteConcern(writeconcern.Majority())
	_, err = session.WithTransaction(ctx, func(tx context.Context) (any, error) {
		for _, userID := range []id.ID{first, second} {
			result, touchErr := m.coll.UpdateOne(tx, withUserId(userID), bson.M{
				"$inc": bson.M{"relationships_version": 1},
			})
			if touchErr != nil {
				return nil, fmt.Errorf("serialize relationship update: %w", touchErr)
			}
			if result.MatchedCount != 1 {
				return nil, ErrUserNotExists
			}
		}
		if mutateErr := mutate(tx); mutateErr != nil {
			return nil, mutateErr
		}
		return struct{}{}, nil
	}, opts)
	if err != nil {
		return fmt.Errorf("commit relationship update: %w", err)
	}
	return nil
}

//nolint:goconst // Literal BSON field and operator names keep query expressions readable.
func (m *mongoUserAdapter) checkFriendCapacity(ctx context.Context, userID, peerID id.ID) error {
	projection := bson.M{
		"count":  bson.M{"$size": bson.M{"$ifNull": bson.A{"$subscribed_users", bson.A{}}}},
		"exists": bson.M{"$in": bson.A{peerID, bson.M{"$ifNull": bson.A{"$subscribed_users", bson.A{}}}}},
	}
	var capacity struct {
		Count  int  `bson:"count"`
		Exists bool `bson:"exists"`
	}
	result := m.coll.FindOne(ctx, withUserId(userID), options.FindOne().SetProjection(projection))
	if err := result.Decode(&capacity); err != nil {
		return fmt.Errorf("read friend capacity: %w", err)
	}
	if capacity.Exists {
		return ErrAlreadyFriends
	}
	if capacity.Count >= MaxFriends {
		return ErrFriendsLimit
	}
	return nil
}

//nolint:goconst // Literal BSON field and operator names keep query expressions readable.
func (m *mongoUserAdapter) SendFriendRequest(ctx context.Context, from, to id.ID) error {
	return m.relationshipTransaction(ctx, from, to, func(tx context.Context) error {
		// Retrying an existing pending request is a no-op, even at capacity.
		err := m.requests.FindOne(tx, bson.M{"from": from, "to": to},
			options.FindOne().SetProjection(bson.M{idField: 1})).Err()
		if err == nil {
			return nil
		}
		if !errors.Is(err, mongo.ErrNoDocuments) {
			return fmt.Errorf("find existing friend request: %w", err)
		}
		for _, pair := range [][2]id.ID{{from, to}, {to, from}} {
			if err := m.checkFriendCapacity(tx, pair[0], pair[1]); err != nil {
				return err
			}
		}
		for _, quota := range []struct {
			field string
			user  id.ID
			limit int
			err   error
		}{
			{"from", from, MaxOutgoingRequests, ErrOutgoingRequestsLimit},
			{"to", to, MaxIncomingRequests, ErrIncomingRequestsLimit},
		} {
			count, countErr := m.requests.CountDocuments(tx, bson.M{quota.field: quota.user},
				options.Count().SetLimit(int64(quota.limit)))
			if countErr != nil {
				return fmt.Errorf("count pending friend requests: %w", countErr)
			}
			if count >= int64(quota.limit) {
				return quota.err
			}
		}
		if err := m.pendingFriendRequestAdapter.SendFriendRequest(tx, from, to); err != nil {
			return fmt.Errorf("store friend request: %w", err)
		}
		return nil
	})
}

//nolint:goconst // Literal BSON field and operator names keep query expressions readable.
func (m *mongoUserAdapter) AcceptFriendRequest(ctx context.Context, userID, requester id.ID) error {
	return m.relationshipTransaction(ctx, userID, requester, func(tx context.Context) error {
		for _, pair := range [][2]id.ID{{userID, requester}, {requester, userID}} {
			if err := m.checkFriendCapacity(tx, pair[0], pair[1]); err != nil {
				return err
			}
		}
		if err := m.DeleteFriendRequest(tx, userID, requester); err != nil {
			return fmt.Errorf("consume friend request: %w", err)
		}
		if err := m.DeleteFriendRequestsBetween(tx, userID, requester); err != nil {
			return fmt.Errorf("clear reverse friend request: %w", err)
		}
		since := m.timer.Now()
		for _, pair := range [][2]id.ID{{userID, requester}, {requester, userID}} {
			_, err := m.coll.UpdateOne(tx, withUserId(pair[0]), mongo.Pipeline{
				{{Key: "$set", Value: bson.M{
					"subscribed_users": bson.M{"$setUnion": bson.A{
						bson.M{"$ifNull": bson.A{"$subscribed_users", bson.A{}}}, bson.A{pair[1]},
					}}, friendSinceKey(pair[1]): since,
				}}},
			})
			if err != nil {
				return fmt.Errorf("store accepted friendship: %w", err)
			}
		}
		return nil
	})
}

//nolint:goconst // Literal BSON field and operator names keep query expressions readable.
func (m *mongoUserAdapter) UnfriendUser(ctx context.Context, userID, peerID id.ID) error {
	return m.relationshipTransaction(ctx, userID, peerID, func(tx context.Context) error {
		if err := m.DeleteFriendRequestsBetween(tx, userID, peerID); err != nil {
			return fmt.Errorf("clear friendship requests: %w", err)
		}
		for _, pair := range [][2]id.ID{{userID, peerID}, {peerID, userID}} {
			fields := bson.M{}
			for _, field := range []string{"subscribed_users", pausedUsersField} {
				fields[field] = bson.M{"$setDifference": bson.A{
					bson.M{"$ifNull": bson.A{"$" + field, bson.A{}}}, bson.A{pair[1]},
				}}
			}
			_, err := m.coll.UpdateOne(tx, withUserId(pair[0]), mongo.Pipeline{
				{{Key: "$set", Value: fields}}, {{Key: "$unset", Value: friendSinceKey(pair[1])}},
			})
			if err != nil {
				return fmt.Errorf("remove friendship: %w", err)
			}
		}
		return nil
	})
}

func (m *mongoUserAdapter) RejectFriendRequest(ctx context.Context, userID, requester id.ID) error {
	return m.relationshipTransaction(ctx, userID, requester, func(tx context.Context) error {
		if err := m.DeleteFriendRequestsBetween(tx, userID, requester); err != nil {
			return fmt.Errorf("reject friendship requests: %w", err)
		}
		return nil
	})
}

func (m *mongoUserAdapter) AddFriend(ctx context.Context, userID, peerID id.ID) error {
	filter := withUserId(userID)
	filter["$or"] = bson.A{
		bson.M{"subscribed_users": peerID},
		bson.M{"$expr": bson.M{"$lt": bson.A{
			bson.M{"$size": bson.M{"$ifNull": bson.A{"$subscribed_users", bson.A{}}}}, MaxFriends,
		}}},
	}
	result, err := m.coll.UpdateOne(ctx, filter, mongo.Pipeline{
		{{Key: "$set", Value: bson.M{"subscribed_users": bson.M{"$setUnion": bson.A{
			bson.M{"$ifNull": bson.A{"$subscribed_users", bson.A{}}}, bson.A{peerID},
		}}}}},
	})
	if err != nil {
		return fmt.Errorf("add friend: %w", err)
	}
	if result.MatchedCount == 0 {
		return ErrFriendsLimit
	}
	return nil
}
