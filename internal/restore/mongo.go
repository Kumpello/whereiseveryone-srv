package restore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"whereiseveryone/pkg/id"
)

const (
	maxKeysPerDevice  = 5
	maxKeysPerAccount = 20
	idField           = "_id"
	expiresAtField    = "expires_at"
	lastUsedAtField   = "last_used_at"
	userIDField       = "user_id"
	revisionField     = "revision"
	greaterThan       = "$gt"
)

// NewMongoStore keeps restore keys separate from passwords and auth sessions.
func NewMongoStore(db *mongo.Database) *MongoStore {
	return &MongoStore{keys: db.Collection("restore_keys"), challenges: db.Collection("restore_challenges"),
		accounts: db.Collection("restore_accounts")}
}

// EnsureIndexes enables expiry cleanup and bounded account-key queries.
func (m *MongoStore) EnsureIndexes(ctx context.Context) error {
	for _, collection := range []*mongo.Collection{m.keys, m.challenges} {
		if _, err := collection.Indexes().CreateOne(ctx, mongo.IndexModel{
			Keys: bson.D{{Key: expiresAtField, Value: 1}}, Options: options.Index().SetExpireAfterSeconds(0),
		}); err != nil {
			return fmt.Errorf("create restore expiry index: %w", err)
		}
	}
	if _, err := m.keys.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: userIDField, Value: 1}, {Key: lastUsedAtField, Value: 1}},
	}); err != nil {
		return fmt.Errorf("create restore account index: %w", err)
	}
	return nil
}

// SaveChallenge persists a ceremony so any server replica can complete it.
func (m *MongoStore) SaveChallenge(ctx context.Context, challenge Challenge) error {
	if _, err := m.challenges.InsertOne(ctx, challenge); err != nil {
		return fmt.Errorf("save restore challenge: %w", err)
	}
	return nil
}

// TakeChallenge atomically consumes an unexpired, correctly bound ceremony.
func (m *MongoStore) TakeChallenge(ctx context.Context, challengeID, purpose, app, deviceDigest string,
	userID id.ID, now time.Time,
) (Challenge, error) {
	var challenge Challenge
	err := m.challenges.FindOneAndDelete(ctx, bson.M{
		idField: challengeID, "purpose": purpose, "app_id": app, "device_digest": deviceDigest,
		userIDField: userID, expiresAtField: bson.M{greaterThan: now},
	}).Decode(&challenge)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Challenge{}, ErrRejected
	}
	if err != nil {
		return Challenge{}, fmt.Errorf("consume restore challenge: %w", err)
	}
	return challenge, nil
}

// FindKey resolves an active credential only within its application namespace.
func (m *MongoStore) FindKey(ctx context.Context, app string, credentialID []byte, now time.Time) (Key, error) {
	var key Key
	filter := bson.M{idField: keyID(app, credentialID), expiresAtField: bson.M{greaterThan: now}}
	err := m.keys.FindOne(ctx, filter).Decode(&key)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Key{}, ErrRejected
	}
	if err != nil {
		return Key{}, fmt.Errorf("find restore key: %w", err)
	}
	return key, nil
}

// SaveKey enrolls a key while atomically enforcing per-device/account caps.
func (m *MongoStore) SaveKey(ctx context.Context, key Key) error {
	// Serialize enrollment per account even on multiple server replicas. The
	// transaction prevents simultaneous registrations from bypassing key caps.
	if _, err := m.accounts.UpdateOne(ctx, bson.M{idField: key.UserID}, bson.M{"$setOnInsert": bson.M{revisionField: 0}},
		options.UpdateOne().SetUpsert(true)); err != nil && !mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("prepare restore account: %w", err)
	}
	session, err := m.keys.Database().Client().StartSession()
	if err != nil {
		return fmt.Errorf("start restore transaction: %w", err)
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(transactionCtx context.Context) (any, error) {
		if _, updateErr := m.accounts.UpdateOne(transactionCtx, bson.M{idField: key.UserID},
			bson.M{"$inc": bson.M{revisionField: 1}}); updateErr != nil {
			return nil, fmt.Errorf("lock restore account: %w", updateErr)
		}
		if _, insertErr := m.keys.InsertOne(transactionCtx, key); insertErr != nil {
			return nil, fmt.Errorf("save restore key: %w", insertErr)
		}
		deviceFilter := bson.M{userIDField: key.UserID, "app_id": key.AppID, "device_digest": key.DeviceDigest}
		if pruneErr := m.pruneKeys(transactionCtx, deviceFilter, maxKeysPerDevice, key.ID); pruneErr != nil {
			return nil, pruneErr
		}
		if pruneErr := m.pruneKeys(transactionCtx, bson.M{userIDField: key.UserID},
			maxKeysPerAccount, key.ID); pruneErr != nil {
			return nil, pruneErr
		}
		return true, nil
	})
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrRejected
		}
		return fmt.Errorf("register restore key: %w", err)
	}
	return nil
}

// UseKey saves counter/flag changes once and renews inactivity expiry.
func (m *MongoStore) UseKey(ctx context.Context, previous Key, credentialJSON []byte, now time.Time) error {
	result, err := m.keys.UpdateOne(ctx, bson.M{
		idField: previous.ID, revisionField: previous.Revision,
		expiresAtField: bson.M{greaterThan: now},
	}, bson.M{"$set": bson.M{"credential_json": credentialJSON,
		lastUsedAtField: now, expiresAtField: now.Add(KeyLifetime)}, "$inc": bson.M{revisionField: 1}})
	if err != nil {
		return fmt.Errorf("update used restore key: %w", err)
	}
	if result.MatchedCount != 1 {
		return ErrRejected
	}
	return nil
}

func (m *MongoStore) pruneKeys(ctx context.Context, filter bson.M, limit int, keepID string) error {
	filter[idField] = bson.M{"$ne": keepID}
	cursor, err := m.keys.Find(ctx, filter, options.Find().SetSort(bson.D{
		{Key: lastUsedAtField, Value: -1}, {Key: idField, Value: -1},
	}).SetSkip(int64(limit-1)).SetProjection(bson.M{idField: 1}))
	if err != nil {
		return fmt.Errorf("list obsolete restore keys: %w", err)
	}
	var obsolete []struct {
		ID string `bson:"_id"` //nolint:tagliatelle // MongoDB identifier.
	}
	if err := cursor.All(ctx, &obsolete); err != nil {
		return fmt.Errorf("read obsolete restore keys: %w", err)
	}
	ids := make([]string, 0, len(obsolete))
	for _, key := range obsolete {
		ids = append(ids, key.ID)
	}
	if len(ids) > 0 {
		if _, err := m.keys.DeleteMany(ctx, bson.M{idField: bson.M{"$in": ids}}); err != nil {
			return fmt.Errorf("prune obsolete restore keys: %w", err)
		}
	}
	return nil
}

// Store persists one-use ceremonies and bounded, independently expiring keys.
type Store interface {
	SaveChallenge(context.Context, Challenge) error
	TakeChallenge(context.Context, string, string, string, string, id.ID, time.Time) (Challenge, error)
	FindKey(context.Context, string, []byte, time.Time) (Key, error)
	SaveKey(context.Context, Key) error
	UseKey(context.Context, Key, []byte, time.Time) error
}

// MongoStore implements restore persistence across server replicas.
type MongoStore struct {
	keys, challenges, accounts *mongo.Collection
}

// Challenge binds a WebAuthn ceremony to its purpose, app, device and account.
type Challenge struct {
	ID           string               `bson:"_id"` //nolint:tagliatelle // MongoDB identifier.
	Purpose      string               `bson:"purpose"`
	AppID        string               `bson:"app_id"`
	DeviceDigest string               `bson:"device_digest"`
	UserID       id.ID                `bson:"user_id"`
	Session      webauthn.SessionData `bson:"session"`
	ExpiresAt    time.Time            `bson:"expires_at"`
}

// Key stores only public credential material and its source-device mapping.
type Key struct {
	ID                 string    `bson:"_id"` //nolint:tagliatelle // MongoDB identifier.
	CredentialID       []byte    `bson:"credential_id"`
	UserID             id.ID     `bson:"user_id"`
	AppID              string    `bson:"app_id"`
	DeviceDigest       string    `bson:"device_digest"`
	CredentialJSON     []byte    `bson:"credential_json"`
	RegistrationDigest string    `bson:"registration_digest"`
	Revision           int64     `bson:"revision"`
	LastUsedAt         time.Time `bson:"last_used_at"`
	ExpiresAt          time.Time `bson:"expires_at"`
}
