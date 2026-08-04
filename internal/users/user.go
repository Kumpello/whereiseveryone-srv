package users

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/logger"
	"whereiseveryone/pkg/pointers"
	"whereiseveryone/pkg/timer"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type User struct {
	// ID is internal ID
	ID id.ID `bson:"_id"` //nolint:tagliatelle // mongo-id
	// Auth is auth details for the user (JWT)
	Auth Auth `bson:"auth"`
	// Location user last location (can be nil)
	Location *Location `bson:"location"`

	// User text status (can be empty)
	Status string `bson:"status"`

	// ObservedUsers list of IDs user subscribe
	SubscribedUsers []id.ID `bson:"subscribed_users"`
	// FriendSince maps subscribed user IDs to the friendship creation time.
	FriendSince map[string]time.Time `bson:"friend_since,omitempty"`
	// PausedUsers list of IDs user has paused sharing location with
	PausedUsers []id.ID `bson:"paused_users"`
}

func (u User) SubscribeUser(id id.ID) bool {
	return slices.Contains(u.SubscribedUsers, id)
}

func (u User) FriendSinceFor(friendID id.ID) *time.Time {
	if u.FriendSince == nil {
		return nil
	}
	key := friendID.Hex()
	if t, ok := u.FriendSince[key]; ok {
		return &t
	}
	return nil
}

func friendSinceKey(friendID id.ID) string {
	return "friend_since." + friendID.Hex()
}

type Adapter interface {
	locationAdapter
	authAdapter

	NewUser(ctx context.Context, user User) (User, error)

	GetUser(ctx context.Context, userID id.ID) (User, error)
	GetUsers(ctx context.Context, ids []id.ID) ([]User, error)
	GetUserByUsername(ctx context.Context, username string) (User, error)
	GetPendingIncomingFriendRequestUserIDs(ctx context.Context, user id.ID) ([]id.ID, error)
	GetPendingOutgoingFriendRequestUserIDs(ctx context.Context, user id.ID) ([]id.ID, error)

	UpdateStatus(ctx context.Context, user id.ID, newStatus string) error
	AddFriend(ctx context.Context, user id.ID, userToObserve id.ID) error
	SetFriendSince(ctx context.Context, user id.ID, friendID id.ID, friendSince time.Time) error
	SendFriendRequest(ctx context.Context, from id.ID, to id.ID) error
	AcceptFriendRequest(ctx context.Context, user id.ID, requester id.ID) error
	RejectFriendRequest(ctx context.Context, user id.ID, requester id.ID) error
	UnfriendUser(ctx context.Context, user id.ID, userToUnfriend id.ID) error
	StopSharing(ctx context.Context, user id.ID, userToPause id.ID) error
	ResumeSharing(ctx context.Context, user id.ID, userToUnpause id.ID) error
}

var ErrUserNotExists = mongo.ErrNoDocuments
var ErrUserNameAlreadyExists = errors.New("username is already in use")
var ErrFriendRequestNotExists = errors.New("friend request does not exist")

type mongoUserAdapter struct {
	locationAdapter
	authAdapter
	pendingFriendRequestAdapter

	coll   *mongo.Collection
	logger logger.Logger
	timer  timer.Timer
}

func NewMongoAdapter(
	coll *mongo.Collection,
	pendingFriendRequestsColl *mongo.Collection,
	timer timer.Timer,
	logger logger.Logger,
) *mongoUserAdapter {
	locationAdapter := mongoLocationAdapter{coll, logger}
	authAdapter := mongoAuthAdapter{coll, timer, logger}
	pendingFriendRequestAdapter := mongoPendingFriendRequestAdapter{pendingFriendRequestsColl, logger}

	return &mongoUserAdapter{
		locationAdapter:             locationAdapter,
		authAdapter:                 authAdapter,
		pendingFriendRequestAdapter: pendingFriendRequestAdapter,
		coll:                        coll,
		logger:                      logger,
		timer:                       timer,
	}
}

func (m *mongoUserAdapter) EnsureIndexes(ctx context.Context) error {
	unique := options.IndexOptions{
		Unique: pointers.Pointer(true),
	}
	userIDIdx := mongo.IndexModel{
		Keys: bson.M{
			"auth.username": 1,
		},
		Options: &unique,
	}

	_, err := m.coll.Indexes().CreateOne(ctx, userIDIdx)
	if err != nil {
		return fmt.Errorf("create unique name:1 index: %w", err)
	}

	m.logger.Infof("Created index on field `auth.username`")

	if err := m.pendingFriendRequestAdapter.EnsureIndexes(ctx); err != nil {
		return fmt.Errorf("ensure pending friend request indexes: %w", err)
	}

	return nil
}

func (m *mongoUserAdapter) NewUser(ctx context.Context, user User) (User, error) {
	user.ID = id.NewID()
	_, err := m.coll.InsertOne(ctx, user)
	if err != nil {
		var writeErr mongo.WriteException
		if errors.As(err, &writeErr) {
			for _, innerErr := range writeErr.WriteErrors {
				if innerErr.Code == 11000 { // duplicate err
					return User{}, ErrUserNameAlreadyExists
				}
			}
		}
		return User{}, fmt.Errorf("create a new user: %w", err)
	}

	return user, nil
}

func (m *mongoUserAdapter) GetUser(ctx context.Context, userID id.ID) (User, error) {
	filter := withUserId(userID)

	res := m.coll.FindOne(ctx, filter)
	if err := res.Err(); err != nil {
		return User{}, fmt.Errorf("find user by id: %w", err)
	}

	var user User
	if err := res.Decode(&user); err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}

	return user, nil
}

func (m *mongoUserAdapter) GetUsers(ctx context.Context, ids []id.ID) ([]User, error) {
	if len(ids) == 0 {
		return []User{}, nil
	}

	filter := bson.M{
		"_id": bson.M{
			"$in": ids,
		},
	}
	opts := options.Find().SetProjection(bson.M{
		"auth.username":      1,
		"location":           1,
		"status":             1,
		"paused_users":       1,
	})
	c, err := m.coll.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("perform find query: %w", err)
	}
	defer c.Close(ctx)

	users := make([]User, 0, len(ids))
	for c.Next(ctx) {
		var user User
		if err := c.Decode(&user); err != nil {
			return nil, fmt.Errorf("decode query result: %w", err)
		}
		users = append(users, user)
	}
	if err := c.Err(); err != nil {
		return nil, fmt.Errorf("read query result: %w", err)
	}

	return users, nil
}

func (m *mongoUserAdapter) GetUserByUsername(ctx context.Context, name string) (User, error) {
	filter := bson.M{
		"auth.username": name,
	}

	var user User
	err := m.coll.FindOne(ctx, filter).Decode(&user)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return User{}, ErrUserNotExists
		}
		return User{}, fmt.Errorf("find user by username: %w", err)
	}

	return user, nil
}

func (m *mongoUserAdapter) UpdateStatus(ctx context.Context, userId id.ID, newStatus string) error {
	filter := withUserId(userId)
	update := bson.M{
		"$set": bson.D{
			{Key: "status", Value: newStatus},
		},
	}

	_, err := m.coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("update user's status: %w", err)
	}

	return nil
}

func (m *mongoUserAdapter) AddFriend(
	ctx context.Context,
	user id.ID,
	userToObserve id.ID,
) error {
	filter := withUserId(user)

	update := bson.M{
		"$addToSet": bson.M{
			"subscribed_users": userToObserve,
		},
	}

	_, err := m.coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("observe user: %w", err)
	}

	return nil
}

func (m *mongoUserAdapter) SetFriendSince(
	ctx context.Context,
	user id.ID,
	friendID id.ID,
	friendSince time.Time,
) error {
	filter := withUserId(user)
	update := bson.M{
		"$set": bson.M{
			friendSinceKey(friendID): friendSince,
		},
	}

	_, err := m.coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("set friend since: %w", err)
	}

	return nil
}

func (m *mongoUserAdapter) SendFriendRequest(
	ctx context.Context,
	from id.ID,
	to id.ID,
) error {
	return m.pendingFriendRequestAdapter.SendFriendRequest(ctx, from, to)
}

func (m *mongoUserAdapter) UnfriendUser(
	ctx context.Context,
	user id.ID,
	userToUnfriend id.ID,
) error {
	if err := m.pendingFriendRequestAdapter.DeleteFriendRequestsBetween(ctx, user, userToUnfriend); err != nil {
		return err
	}

	_, err := m.coll.BulkWrite(ctx, []mongo.WriteModel{
		mongo.NewUpdateOneModel().
			SetFilter(withUserId(user)).
			SetUpdate(bson.M{
				"$pull": bson.M{
					"subscribed_users": userToUnfriend,
				},
				"$unset": bson.M{
					friendSinceKey(userToUnfriend): "",
				},
			}),
		mongo.NewUpdateOneModel().
			SetFilter(withUserId(userToUnfriend)).
			SetUpdate(bson.M{
				"$pull": bson.M{
					"subscribed_users": user,
				},
				"$unset": bson.M{
					friendSinceKey(user): "",
				},
			}),
	})
	if err != nil {
		return fmt.Errorf("remove friendship: %w", err)
	}

	return nil
}

func (m *mongoUserAdapter) AcceptFriendRequest(
	ctx context.Context,
	user id.ID,
	requester id.ID,
) error {
	err := m.DeleteFriendRequest(ctx, user, requester)
	if err != nil {
		return err
	}

	friendSince := m.timer.Now()

	_, err = m.coll.BulkWrite(ctx, []mongo.WriteModel{
		mongo.NewUpdateOneModel().
			SetFilter(withUserId(user)).
			SetUpdate(bson.M{
				"$addToSet": bson.M{
					"subscribed_users": requester,
				},
				"$set": bson.M{
					friendSinceKey(requester): friendSince,
				},
			}),
		mongo.NewUpdateOneModel().
			SetFilter(withUserId(requester)).
			SetUpdate(bson.M{
				"$addToSet": bson.M{
					"subscribed_users": user,
				},
				"$set": bson.M{
					friendSinceKey(user): friendSince,
				},
			}),
	})
	if err != nil {
		return fmt.Errorf("accept friend request: %w", err)
	}

	return nil
}

func (m *mongoUserAdapter) RejectFriendRequest(
	ctx context.Context,
	user id.ID,
	requester id.ID,
) error {
	if err := m.pendingFriendRequestAdapter.DeleteFriendRequestsBetween(ctx, user, requester); err != nil {
		return err
	}

	return nil
}

func (m *mongoUserAdapter) StopSharing(ctx context.Context, userID id.ID, userToPause id.ID) error {
	filter := withUserId(userID)
	update := bson.M{
		"$addToSet": bson.M{
			"paused_users": userToPause,
		},
	}

	_, err := m.coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("stop sharing location: %w", err)
	}

	return nil
}

func (m *mongoUserAdapter) ResumeSharing(ctx context.Context, userID id.ID, userToUnpause id.ID) error {
	filter := withUserId(userID)
	update := bson.M{
		"$pull": bson.M{
			"paused_users": userToUnpause,
		},
	}

	_, err := m.coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("start sharing location: %w", err)
	}

	return nil
}

var _ Adapter = (*mongoUserAdapter)(nil)
