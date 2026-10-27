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

// MaxPointsOfInterest is enforced atomically on each owner's document.
const MaxPointsOfInterest = 5

const pointsOfInterestField = "points_of_interest"

// ErrPointsOfInterestLimit indicates that the owner has no free point slots.
var ErrPointsOfInterestLimit = errors.New("a user can save at most five points of interest")

// ErrPointOfInterestNotFound also covers points belonging to another owner.
var ErrPointOfInterestNotFound = errors.New("point of interest not found")

// PointOfInterest is a saved, stationary destination owned by its containing user.
type PointOfInterest struct {
	ID          id.ID     `bson:"_id"` //nolint:tagliatelle // MongoDB document identifier.
	Name        string    `bson:"name"`
	Description string    `bson:"description"`
	Latitude    float64   `bson:"latitude"`
	Longitude   float64   `bson:"longitude"`
	CreatedAt   time.Time `bson:"created_at"`
}

// GetPointsOfInterest reads only the authenticated owner's saved points.
func (m *mongoUserAdapter) GetPointsOfInterest(ctx context.Context, owner id.ID) ([]PointOfInterest, error) {
	var result struct {
		Points []PointOfInterest `bson:"points_of_interest"`
	}
	opts := options.FindOne().SetProjection(bson.M{idField: 0, pointsOfInterestField: 1})
	if err := m.coll.FindOne(ctx, withUserId(owner), opts).Decode(&result); err != nil {
		return nil, fmt.Errorf("read own points of interest: %w", err)
	}
	if result.Points == nil {
		return []PointOfInterest{}, nil
	}
	return result.Points, nil
}

// CreatePointOfInterest appends only while the fifth slot is absent. Concurrent
// creates cannot exceed the limit; IDs and creation times always come from the server.
func (m *mongoUserAdapter) CreatePointOfInterest(
	ctx context.Context, owner id.ID, point PointOfInterest,
) (PointOfInterest, error) {
	point.ID = id.NewID()
	point.CreatedAt = m.timer.Now().UTC().Truncate(time.Millisecond)
	filter := withUserId(owner)
	filter[fmt.Sprintf("%s.%d", pointsOfInterestField, MaxPointsOfInterest-1)] = bson.M{"$exists": false}
	result, err := m.coll.UpdateOne(ctx, filter, bson.M{"$push": bson.M{pointsOfInterestField: point}})
	if err != nil {
		return PointOfInterest{}, fmt.Errorf("create point of interest: %w", err)
	}
	if result.MatchedCount == 0 {
		if _, err := m.GetPointsOfInterest(ctx, owner); err != nil {
			return PointOfInterest{}, err
		}
		return PointOfInterest{}, ErrPointsOfInterestLimit
	}
	return point, nil
}

// UpdatePointOfInterest preserves identity and creation time and scopes the write
// to the authenticated owner, regardless of who can view the point.
//
//nolint:goconst // Literal BSON field and operator names keep query expressions readable.
func (m *mongoUserAdapter) UpdatePointOfInterest(
	ctx context.Context, owner id.ID, pointID id.ID, point PointOfInterest,
) (PointOfInterest, error) {
	filter := withUserId(owner)
	filter[pointsOfInterestField+"._id"] = pointID
	fields := bson.M{
		pointsOfInterestField + ".$.name":        point.Name,
		pointsOfInterestField + ".$.description": point.Description,
		pointsOfInterestField + ".$.latitude":    point.Latitude,
		pointsOfInterestField + ".$.longitude":   point.Longitude,
	}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After).
		SetProjection(bson.M{pointsOfInterestField: bson.M{"$elemMatch": bson.M{idField: pointID}}})
	var result struct {
		Points []PointOfInterest `bson:"points_of_interest"`
	}
	err := m.coll.FindOneAndUpdate(ctx, filter, bson.M{"$set": fields}, opts).Decode(&result)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return PointOfInterest{}, ErrPointOfInterestNotFound
	}
	if err != nil {
		return PointOfInterest{}, fmt.Errorf("update point of interest: %w", err)
	}
	if len(result.Points) != 1 {
		return PointOfInterest{}, ErrPointOfInterestNotFound
	}
	return result.Points[0], nil
}

// DeletePointOfInterest frees one of the authenticated owner's five slots.
func (m *mongoUserAdapter) DeletePointOfInterest(ctx context.Context, owner id.ID, pointID id.ID) error {
	filter := withUserId(owner)
	filter[pointsOfInterestField+"._id"] = pointID
	result, err := m.coll.UpdateOne(ctx, filter, bson.M{
		"$pull": bson.M{pointsOfInterestField: bson.M{idField: pointID}},
	})
	if err != nil {
		return fmt.Errorf("delete point of interest: %w", err)
	}
	if result.MatchedCount == 0 {
		return ErrPointOfInterestNotFound
	}
	return nil
}
