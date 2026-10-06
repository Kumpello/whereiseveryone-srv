package mongo

import (
	"context"
	"fmt"
	"strconv"

	"whereiseveryone/internal/config"
	"whereiseveryone/pkg/env"
)

func GetMongo(appCtx context.Context, env env.Handler) (*Collections, error) {
	mongoUseCloud := env.MustEnv(config.ConfMongoUseCloud)
	useCloud, err := strconv.ParseBool(mongoUseCloud)
	if err != nil {
		return nil, fmt.Errorf("parse mongo.useCloud flag: %w", err)
	}

	if useCloud {
		return CloudMongo(appCtx, env)
	}

	return LocalMongo(appCtx, env)
}

func LocalMongo(appCtx context.Context, env env.Handler) (*Collections, error) {
	poolSize, err := config.MongoMaxPoolSizeFromEnv(env)
	if err != nil {
		return nil, fmt.Errorf("configure mongo pool: %w", err)
	}
	mongoURI := env.MustEnv(config.ConfMongoURI)
	mongoAuthDB := env.MustEnv(config.ConfMongoAuthDb)
	mongoUser := env.MustEnv(config.ConfMongoUser)
	mongoPassword := env.MustEnv(config.ConfMongoPassword)
	mongoDB := env.MustEnv(config.ConfMongoDb)

	mongoCollections, err := NewMongoWithPassword(
		appCtx, mongoDB, mongoURI, mongoAuthDB, mongoUser, mongoPassword, poolSize,
	)
	if err != nil {
		return nil, fmt.Errorf("init mongo: %w", err)
	}
	return mongoCollections, nil
}

func CloudMongo(appCtx context.Context, env env.Handler) (*Collections, error) {
	poolSize, err := config.MongoMaxPoolSizeFromEnv(env)
	if err != nil {
		return nil, fmt.Errorf("configure mongo pool: %w", err)
	}
	mongoURI := env.MustEnv(config.ConfMongoURI)
	mongoCrt := env.MustEnv(config.ConfMongoX509)
	mongoDB := env.MustEnv(config.ConfMongoDb)

	mongoCollections, err := NewMongoWithX509Pem(appCtx, mongoDB, mongoURI, mongoCrt, poolSize)
	if err != nil {
		return nil, fmt.Errorf("init mongo: %w", err)
	}
	return mongoCollections, nil
}
