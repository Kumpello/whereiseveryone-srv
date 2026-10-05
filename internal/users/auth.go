package users

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/logger"
	"whereiseveryone/pkg/timer"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type Auth struct {
	// Username is a unique name of user (username), used for login as well
	Username string `bson:"username"`
	// Password is an encrypted password
	Password string `bson:"password"`
	// Token is a jwt-token
	Token string `bson:"token"`
	// RefreshToken is read only for migration of existing plaintext credentials.
	RefreshToken             string    `bson:"refresh_token,omitempty"`
	RefreshTokenDigest       string    `bson:"refresh_token_digest,omitempty"`
	PreviousAccessDigest     string    `bson:"previous_access_digest,omitempty"`
	PreviousAccessValidUntil time.Time `bson:"previous_access_valid_until,omitempty"`
	// DeviceToken identifies client device (for single-device/session enforcement)
	DeviceToken string `bson:"device_token,omitempty"`
	// CreatedAt tells when the user was created
	CreatedAt time.Time `bson:"created_at"`
	// UpdatedAt tells when the last update was done
	UpdatedAt time.Time `bson:"updated_at"`
}

type authAdapter interface {
	// GetSession reads only the credentials needed to validate an access session.
	GetSession(ctx context.Context, userID id.ID) (Auth, error)
	// UpdateTokens update user tokens (if they are not nil)
	UpdateTokens(ctx context.Context, userID id.ID, token, refreshedToken, deviceToken *string) error
	// ReplaceTokens consumes exactly the session that was read by the caller.
	ReplaceTokens(ctx context.Context, userID id.ID, previous Auth, token, refresh, device string) error
}

var ErrSessionChanged = errors.New("session changed or refresh token already consumed")

const AccessRotationGrace = 120 * time.Second

const (
	accessTokenField              = "auth.token"
	deviceTokenField              = "auth.device_token" // #nosec G101 -- MongoDB field name, not a credential.
	previousAccessDigestField     = "auth.previous_access_digest"
	previousAccessValidUntilField = "auth.previous_access_valid_until"
)

// TokenDigest hashes a high-entropy signed credential for storage.
func TokenDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func (a Auth) MatchesRefresh(token string) bool {
	if token == "" {
		return false
	}
	if a.RefreshTokenDigest != "" {
		return subtle.ConstantTimeCompare([]byte(a.RefreshTokenDigest), []byte(TokenDigest(token))) == 1
	}
	return subtle.ConstantTimeCompare([]byte(a.RefreshToken), []byte(token)) == 1
}

// MatchesAccess checks session membership; JWT signature and expiration must also be validated.
func (a Auth) MatchesAccess(token string, now time.Time) bool {
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a.Token), []byte(token)) == 1 ||
		(now.Before(a.PreviousAccessValidUntil) &&
			subtle.ConstantTimeCompare([]byte(a.PreviousAccessDigest), []byte(TokenDigest(token))) == 1)
}

type mongoAuthAdapter struct {
	coll   *mongo.Collection
	timer  timer.Timer
	logger logger.Logger
}

func (m mongoAuthAdapter) GetSession(ctx context.Context, userID id.ID) (Auth, error) {
	opts := options.FindOne().SetProjection(bson.M{
		idField:                       0,
		accessTokenField:              1,
		deviceTokenField:              1,
		previousAccessDigestField:     1,
		previousAccessValidUntilField: 1,
	})
	var session struct {
		Auth Auth `bson:"auth"`
	}
	if err := m.coll.FindOne(ctx, withUserId(userID), opts).Decode(&session); err != nil {
		return Auth{}, fmt.Errorf("get session: %w", err)
	}
	return session.Auth, nil
}

func (m mongoAuthAdapter) UpdateTokens(ctx context.Context, userID id.ID, token, refreshedToken, deviceToken *string) error {
	tokens := bson.D{}
	if token != nil {
		tokens = append(tokens, bson.E{Key: accessTokenField, Value: *token})
	}
	if refreshedToken != nil {
		tokens = append(tokens, bson.E{Key: "auth.refresh_token_digest", Value: TokenDigest(*refreshedToken)})
	}
	if deviceToken != nil {
		tokens = append(tokens, bson.E{Key: deviceTokenField, Value: *deviceToken})
	}
	if len(tokens) == 0 {
		// nothing to update
		return nil
	}

	tokens = append(tokens, bson.E{Key: "auth.updated_at", Value: m.timer.Now()})
	filter := withUserId(userID)
	unset := bson.M{previousAccessDigestField: "", previousAccessValidUntilField: ""}
	update := bson.M{
		"$set":   tokens,
		"$unset": unset,
	}
	if refreshedToken != nil {
		unset["auth.refresh_token"] = ""
	}

	result, err := m.coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("update tokens: %w", err)
	}
	if result.MatchedCount != 1 {
		return ErrSessionChanged
	}

	return nil
}

func (m mongoAuthAdapter) ReplaceTokens(ctx context.Context, userID id.ID, previous Auth, token, refresh, device string) error {
	filter := withUserId(userID)
	filter[accessTokenField] = previous.Token
	filter[deviceTokenField] = previous.DeviceToken
	if previous.RefreshTokenDigest != "" {
		filter["auth.refresh_token_digest"] = previous.RefreshTokenDigest
	} else {
		// Only legacy sessions may match plaintext. A migrated session cannot fall back.
		filter["auth.refresh_token"] = previous.RefreshToken
		filter["auth.refresh_token_digest"] = bson.M{"$in": bson.A{nil, ""}}
	}
	now := m.timer.Now()
	set := bson.M{
		accessTokenField:            token,
		"auth.refresh_token_digest": TokenDigest(refresh),
		deviceTokenField:            device,
		"auth.updated_at":           now,
	}
	unset := bson.M{"auth.refresh_token": ""}
	if device != "" && device == previous.DeviceToken {
		set[previousAccessDigestField] = TokenDigest(previous.Token)
		set[previousAccessValidUntilField] = now.Add(AccessRotationGrace)
	} else {
		unset[previousAccessDigestField] = ""
		unset[previousAccessValidUntilField] = ""
	}
	result, err := m.coll.UpdateOne(ctx, filter, bson.M{"$set": set, "$unset": unset})
	if err != nil {
		return fmt.Errorf("replace tokens: %w", err)
	}
	if result.MatchedCount != 1 {
		return ErrSessionChanged
	}
	return nil
}

var _ authAdapter = (*mongoAuthAdapter)(nil)
