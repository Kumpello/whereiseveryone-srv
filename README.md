# Run

## Go

It's a standard go app. You can run it using `go run` etc.

## Config

App uses json-config. The config MUST be a JSON with **only string** entries.
As a default `./.env/local.json` is used. You can override it with a flag `--config=$filePath`

Only templates (`.env/*.example.json`) are checked in. Create the configuration you
need with a fresh JWT signing secret:

```sh
go run ./cmd/cli initConfig --template=./.env/local.example.json --config=./.env/local.json
# For separate cloud or Docker environments, generate independently:
go run ./cmd/cli initConfig --template=./.env/cloud.example.json --config=./.env/cloud.json
go run ./cmd/cli initConfig --template=./.env/docker.example.json --config=./.env/docker.json
```

Each invocation generates 32 cryptographically random bytes encoded as base64,
writes a private file (mode `0600`), and refuses to overwrite an existing path.
Fill in the database user/password or Atlas hostname and certificate path before
starting the server. The command does not provision MongoDB accounts. Keep a
different signing secret for each environment; replicas of the same environment
must share its secret. Generated configuration and certificates are Git-ignored.

Startup rejects missing, blank, shorter-than-32-byte, and template JWT secrets,
as well as secrets with surrounding whitespace, before connecting to MongoDB.
The old committed 13-byte secret is rejected. Length checks cannot establish
randomness: use the generator or a secret manager with an equivalent random key.

For an existing deployment, generate a new configuration at a new private path,
apply its deployment-specific settings, then switch the deployment to it. Rotate
any copy of the previously committed signing secret: it remains exposed in Git
history. Rotation invalidates existing access and refresh tokens, so users must
log in again. For Compose, provide an ignored `secrets.env` with MongoDB credentials
matching `.env/docker.json`.

Optional performance-related config:

* `app.bcryptCost` - bcrypt work factor for new password hashes. Defaults to `14`.

## Docker - srv

To build an image locally run: `docker build -t whereiseveryone-srv:latest .` in project root.
Runtime configuration is excluded from both production and debug images. Mount
the private `.env` directory read-only at runtime; this is also required for `Dockerfile.debug`.

If you want to use local-running mongo (see section `Development/Mongo` below) in docker a network must be created at
first.

```
docker network create whereiseveryone-net
docker network connect whereiseveryone-net mongodb
```

Then, to run an image:

```
docker run \
    -p 127.0.0.1:8080:8080/tcp \
    --network=whereiseveryone-net \
    -v "`pwd`/.env:/app/.env:ro" \
    whereiseveryone-srv \
    /bin/sh -c "/app/app-srv --config=/app/.env/docker.json"
```

The command:

* `-p` binds docker to your localhost on port 8080
* `--network` connects the container to the network (required for connecting with local-docker mongo)
* `-v` binds local `./env` directory to container `.env` directory
* `/bin/sh -c ...` command for running the srv with docker-config file

## Docker - cli

On default app image there is a second binary - `cli`. This is a command line app that allows to execute some admin
commands, like db-management. To use it:

```
docker run \
    --network=whereiseveryone-net \
    -v "`pwd`/.env:/app/.env:ro" \
    whereiseveryone-srv \
    /bin/sh -c "/app/app-cli --config=/app/.env/docker.json <command>"
```

To see list of available commands just run the client without any command.
The server also verifies Mongo indexes during startup; the CLI command remains useful for explicit migration/preflight jobs.

# Authorization

- All users are required to create an account.
- For authentication a JWT token is required.
- To signup use `/auth/signup`

```go
package auth

type signUpRequest struct {
	Name     string `json:"name" validate:"required"`
	Password string `json:"password" validate:"required"`
}

type logInRequest struct {
	Name     string `json:"name" validate:"required"`
	Password string `json:"password" validate:"required"`
}

type authResponse struct {
	ID           string `json:"id"`
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
}
```

All requests (except `/auth/*`) are required to have JWT token attached (`Header -> Authorization: Bearer <<token>>`).
Protected routes accept only access tokens with `token_use=access`, and check that the
token matches the user's current stored access token on every request. Refresh tokens
with `token_use=refresh` can only be used with `/auth/refresh`, which also checks the
stored refresh token. Login, refresh, and device-conflict token replacement invalidate
previous access tokens for subsequent requests. Tokens have random IDs so replacement
also works within the same second. Requests already authorized may finish.

When an access token expires, renew the pair with `/auth/refresh` or log in again using
`/auth/login`. Expired tokens return 401; wrong-purpose or revoked tokens return 403.
Session-store failures deny access with 500.

Deploying token-purpose validation invalidates all previously issued tokens without
`token_use`; existing users must log in again. No database migration or new index is
required. Protected requests now require an available MongoDB session lookup.

# Development

To run app in development, at first run MongoDB docker container:

Create the network for further use:
`docker network create whereiseveryone-net`

Then you can start the Mongo instance:
`docker run --network=whereiseveryone-net --name mongodb -p 27017:27017 -e MONGODB_ROOT_PASSWORD=password123 bitnami/mongodb:4.4`

You can run MongoExpress (WEB GUI for the database) too:
```
docker run --network=whereiseveryone-net \
    -e ME_CONFIG_MONGODB_SERVER=mongodb \
    -e ME_CONFIG_MONGODB_ADMINUSERNAME=root \
    -e ME_CONFIG_MONGODB_ADMINPASSWORD=password123 \
    -p 8081:8081 mongo-express
```

Credentials for MongoExpress is `admin:pass`


This command will run the mongodb container with root user: `root:password123` on port 27017

## Config

To see a list of available config keys please see `/internal/config/dict.go`.
For local development use `./.env/local.json` file.

Select another configuration with `--config=path/to/private.json`.

## Using cloud db

At first, you need to generate a X509 certificate from Mongo Atlas. **Keep it secret!**
Put it in `.env` and generate `./.env/cloud.json` from the cloud template using
`initConfig` above. Set the Atlas hostname and certificate path in the generated file.

# Documentation

Docs are served in /swagger endpoint.
Ref: https://github.com/swaggo/echo-swagger

For generating docs (required each time something is changed) `swag init -g cmd/server/main.go`
and commit it to the repository.

## Binding Requests

There is a very useful generic function that binds the HTTP request and validates it.

```go
package request

func echoFunc(c echo.Context) error {
	data, bindErr := binder.BindRequest[bodyType](c, true)
	if bindErr != nil {
		return c.String(bindErr.Code, bindErr.Message)
	}
	defer data.Cancel()

	return c.String(200, "ok")
}
```

`BindRequest` returns an object implementing the interface

```go
package request

type BaseContext interface {
	Context() context.Context
	Cancel()
	Echo() echo.Context
	UserID() id.ID
	TokenData() jwt.SignedToken
}
```

# Production

TBD.
