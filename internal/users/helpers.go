package users

import (
	"go.mongodb.org/mongo-driver/v2/bson"
	"whereiseveryone/pkg/id"
)

const (
	idField          = "_id"
	locationField    = "location"
	pausedUsersField = "paused_users"
)

func withUserId(id id.ID) bson.M {
	return bson.M{
		idField: id,
	}
}
