package config

import "whereiseveryone/pkg/env"

const (
	ConfMongoUseCloud env.Key = "mongo.useCloud" // required
	ConfMongoURI      env.Key = "mongo.uri"      // required
	ConfMongoAuthDb   env.Key = "mongo.authDb"   // required for local-db
	ConfMongoUser     env.Key = "mongo.user"     // required for local-db
	ConfMongoPassword env.Key = "mongo.password" // required for local-db
	ConfMongoDb       env.Key = "mongo.db"       // required
	ConfMongoX509     env.Key = "mongo.x509"     // required for cloud

	//nolint:gosec // not a credential
	ConfJwtSecret  env.Key = "app.jwtSecret"  // required
	ConfDebug      env.Key = "app.debug"      // required
	ConfAppPort    env.Key = "app.port"       // required
	ConfBcryptCost env.Key = "app.bcryptCost" // optional, default 14
)
