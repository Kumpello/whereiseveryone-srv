package id

import (
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// ID is the BSON ObjectID used in both stored documents and API identifiers.
type ID = bson.ObjectID

var ZeroID = [12]byte{} //nolint:gochecknoglobals // cannot be const

func NewID() ID {
	return bson.NewObjectID()
}

func FromString(s string) (ID, error) {
	if s == "" {
		return ZeroID, nil
	}

	id, err := bson.ObjectIDFromHex(s)
	if err != nil {
		return ZeroID, fmt.Errorf("parse id: %w", err)
	}
	return id, nil
}
