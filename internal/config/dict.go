package config

import "whereiseveryone/pkg/env"

// MongoDB connection keys; defaults apply when optional keys are omitted.
const (
	ConfMongoUseCloud    env.Key = "mongo.useCloud"    // required
	ConfMongoURI         env.Key = "mongo.uri"         // required
	ConfMongoAuthDb      env.Key = "mongo.authDb"      // required for local-db
	ConfMongoUser        env.Key = "mongo.user"        // required for local-db
	ConfMongoPassword    env.Key = "mongo.password"    // required for local-db
	ConfMongoDb          env.Key = "mongo.db"          // required
	ConfMongoX509        env.Key = "mongo.x509"        // required for cloud
	ConfMongoMaxPoolSize env.Key = "mongo.maxPoolSize" // optional, default 8 per MongoDB server
)

// Application configuration keys; defaults apply when optional keys are omitted.
const (
	//nolint:gosec // not a credential
	ConfJwtSecret               env.Key = "app.jwtSecret"               // required
	ConfDebug                   env.Key = "app.debug"                   // required
	ConfAppPort                 env.Key = "app.port"                    // required
	ConfBcryptCost              env.Key = "app.bcryptCost"              // optional, default 14
	ConfMaxConcurrentDBRequests env.Key = "app.maxConcurrentDBRequests" // optional, default 4
	ConfDBRequestTimeoutSeconds env.Key = "app.dbRequestTimeoutSeconds" // optional, default 15
)
