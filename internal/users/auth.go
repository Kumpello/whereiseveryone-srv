package users

import (
	"context"
	"fmt"
	"time"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/logger"
	"whereiseveryone/pkg/timer"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type Auth struct {
	// Username is a unique name of user (username), used for login as well
	Username string `bson:"username"`
	// Password is an encrypted password
	Password string `bson:"password"`
	// Token is a jwt-token
	Token string `bson:"token"`
	// RefreshToken is jwt-refresh-token
	RefreshToken string `bson:"refresh_token"`
	// DeviceToken identifies client device (for single-device/session enforcement)
	DeviceToken string `bson:"device_token,omitempty"`
	// CreatedAt tells when the user was created
	CreatedAt time.Time `bson:"created_at"`
	// UpdatedAt tells when the last update was done
	UpdatedAt time.Time `bson:"updated_at"`
}

type authAdapter interface {
	// UpdateTokens update user tokens (if they are not nil)
	UpdateTokens(ctx context.Context, userID id.ID, token, refreshedToken, deviceToken *string) error
}

type mongoAuthAdapter struct {
	coll   *mongo.Collection
	timer  timer.Timer
	logger logger.Logger
}

func (m mongoAuthAdapter) UpdateTokens(ctx context.Context, userID id.ID, token, refreshedToken, deviceToken *string) error {
	tokens := bson.D{}
	if token != nil {
		tokens = append(tokens, bson.E{Key: "auth.token", Value: *token})
	}
	if refreshedToken != nil {
		tokens = append(tokens, bson.E{Key: "auth.refresh_token", Value: *refreshedToken})
	}
	if deviceToken != nil {
		tokens = append(tokens, bson.E{Key: "auth.device_token", Value: *deviceToken})
	}
	tokens = append(tokens, bson.E{Key: "auth.updated_at", Value: m.timer.Now()})

	if len(tokens) == 0 {
		// nothing to update
		return nil
	}

	filter := withUserId(userID)
	update := bson.M{
		"$set": tokens,
	}

	_, err := m.coll.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("update tokens: %w", err)
	}

	return nil
}

var _ authAdapter = (*mongoAuthAdapter)(nil)
