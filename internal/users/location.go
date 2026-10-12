package users

import (
	"context"
	"fmt"
	"time"

	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/logger"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

type Location struct {
	// Longitude
	Longitude float64 `bson:"longitude"`
	// Latitude
	Latitude float64 `bson:"latitude"`
	// Altitude
	Altitude float64 `bson:"altitude,omitempty"`
	// Bearing is GPS course in degrees; nil means the fix has no course measurement.
	Bearing *float64 `bson:"bearing,omitempty"`
	// Accuracy
	Accuracy float64 `bson:"accuracy,omitempty"`
	// Speed in m/s; nil means unavailable, zero means stationary.
	Speed *float64 `bson:"speed,omitempty"`
	// CarMode indicates that the user enabled the driving display.
	CarMode bool `bson:"car_mode"`
	// LastUpdate
	LastUpdate time.Time `bson:"last_update"`
}

type locationAdapter interface {
	UpdateLocation(ctx context.Context, userID id.ID, newLocation Location) error
	WipeLocation(ctx context.Context, userID id.ID) error
}

type mongoLocationAdapter struct {
	userCollection *mongo.Collection

	logger logger.Logger
}

func (l mongoLocationAdapter) UpdateLocation(ctx context.Context, userID id.ID, newLocation Location) error {
	location := bson.D{
		bson.E{Key: locationField, Value: newLocation},
	}

	filter := withUserId(userID)
	// Compare and update atomically so delayed requests and retries cannot overwrite
	// a newer fix. Missing timestamps also cover new users and wiped locations.
	filter["$or"] = bson.A{
		bson.M{locationField + ".last_update": bson.M{"$exists": false}},
		bson.M{locationField + ".last_update": bson.M{"$lt": newLocation.LastUpdate}},
	}
	update := bson.M{
		"$set": location,
	}

	_, err := l.userCollection.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("update user's location: %w", err)
	}

	return nil
}

func (l mongoLocationAdapter) WipeLocation(ctx context.Context, userID id.ID) error {
	filter := withUserId(userID)
	update := bson.M{
		"$set": bson.M{
			locationField: nil,
		},
	}

	_, err := l.userCollection.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("wipe user's location: %w", err)
	}

	return nil
}

var _ locationAdapter = (*mongoLocationAdapter)(nil)
