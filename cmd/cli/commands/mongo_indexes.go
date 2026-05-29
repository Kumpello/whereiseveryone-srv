package commands

import (
	"context"
	"whereiseveryone/internal/users"
)

func (c *commandApp) mongoIndexes(ctx context.Context) {
	envHandler := c.mustGetEnvHandler()
	mongoCollections := c.mustGetMongoCollections(ctx, envHandler)

	usersAdapter := users.NewMongoAdapter(mongoCollections.Users, mongoCollections.PendingFriendRequests, c.timer, c.logger)

	if err := usersAdapter.EnsureIndexes(c.Context()); err != nil {
		c.logger.Fatalf("create indexes on users collection: %s", err.Error())
	}
}
