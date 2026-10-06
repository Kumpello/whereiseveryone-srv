# Run

## Go

It's a standard go app. You can run it using `go run` etc.

Use Go 1.27.1 and MongoDB 4.4 or newer (required by MongoDB driver v2).
The application uses Echo v5, validator v10, and JWT v5; existing JWT and
database ID formats are preserved. Swagger's transitive `github.com/sv-tools/openapi`
dependency stays on v0.4.0 because its v1 releases removed the `spec` package
required by `swag/v2`.

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

* `app.bcryptCost` - bcrypt work factor for new password hashes and dummy verification. Defaults to `14`; supported range is `4`–`14`.
* `app.maxConcurrentPasswordRequests` - shared login/signup password-work cap per instance. Defaults to `1`.
* `app.maxConcurrentDBRequests` - shared admission cap for `/auth/*` and `/me/*`. Defaults to `4`.
* `app.dbRequestTimeoutSeconds` - one database-work deadline per admitted request. Defaults to `15` seconds.
* `mongo.maxPoolSize` - connection-pool cap per MongoDB server. Defaults to `8`, overriding URI `maxPoolSize`.

Values in JSON configuration are strings. The new limits are optional in existing
files; omitted keys use these defaults. Nonpositive or malformed limits reject
startup, including `mongo.maxPoolSize=0` (which would mean unlimited in the driver).
Malformed or out-of-range bcrypt costs also reject startup before connecting to
MongoDB; they no longer fall back silently. Configurations with costs above `14`
must be reviewed before rollout. The supported ceiling is a CPU budget for new
hashes and dummy work, not a change to existing stored hashes.

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
	Username    string `json:"username" validate:"required"`
	Password    string `json:"password" validate:"required,min=8"`
	DeviceToken string `json:"device_token" validate:"required"`
}

type logInRequest struct {
	Username    string `json:"username" validate:"required"`
	Password    string `json:"password" validate:"required"`
	DeviceToken string `json:"device_token" validate:"required"`
}

type refreshTokenRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required"`
	DeviceToken  string `json:"device_token" validate:"required"`
}

type authResponse struct {
	ID           string `json:"id"`
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
}
```

All requests (except `/auth/*`) are required to have JWT token attached (`Header -> Authorization: Bearer <<token>>`).
Protected routes accept only access tokens with `token_use=access`, and check that the
token matches the user's stored session on every request. Refresh tokens with
`token_use=refresh` can only be used with `/auth/refresh`. Rotation atomically matches
the previous refresh credential and device, so only one concurrent request succeeds;
reuse returns 403 immediately. Refresh credentials are stored as SHA-256 digests.
Existing plaintext credentials migrate on the next login or rotation, without an
index change. Tokens have random IDs so replacement also works within the same second.

After refresh, the immediately preceding access token remains valid for up to 120
seconds, ending earlier at its JWT expiration or the next token replacement. Login
and device-conflict revocation clear this grace period immediately. The grace period
does not permit reuse of refresh tokens or extend JWT expiration. Requests already
authorized may finish.

Run database regression tests against an isolated replica set with
`MONGO_TEST_URI='mongodb://127.0.0.1:27028/?replicaSet=rs0&directConnection=true' go test -race ./internal/users`.

When an access token expires, renew the pair with `/auth/refresh` or log in again using
`/auth/login`. Expired tokens return 401; wrong-purpose or revoked tokens return 403.
Session-store failures deny access with 500.

Signup, login, and refresh require a nonblank `device_token`. Missing, null,
empty, or whitespace-only identifiers return 400 without changing the existing
session. Android clients must send their device identifier on all three calls;
refresh must send the same identifier used to establish the session.

A valid login or refresh from a different device returns 409 and invalidates the
existing token pair and device binding. The client must then log in with its
password and device identifier to establish a new session. Refresh cannot establish
a new device binding. Legacy sessions without a device identifier are rejected
with 403 on protected routes and refresh; those users must log in again.

The exact stored access token, including its random token ID, acts as the revocable
session credential, so a separate session-version field or database migration is
not required. The device identifier is supplied by the client and is not proof of
hardware identity: a stolen access token is still a bearer credential until it is
replaced or expires.

Deploying token-purpose validation invalidates all previously issued tokens without
`token_use`; existing users must log in again. No database migration or new index is
required. Protected requests now require an available MongoDB session lookup.

`GET /me/friends` exposes status and eligible location only for accepted
friendships. Incoming and outgoing pending entries omit `status` and `location`;
accepted friends include `status` even when it is empty.

## Public errors

Internal failures return JSON containing `code`, the generic `message`
`internal error`, and `correlation_id`. Underlying errors are never serialized,
including when debug mode is enabled. Every response carries a server-generated
`X-Request-ID`. It matches the ID in the common JSON error body and in the
structured request log, where the underlying error is recorded.
Use this ID when investigating failures. Client-supplied request IDs are ignored.

## Authentication abuse protection

Login returns the same HTTP 403 code and public message for unknown usernames and
incorrect passwords, with a unique correlation ID per request. Both perform
bcrypt verification; malformed stored hashes also perform dummy verification. The dummy hash uses `app.bcryptCost`. Lower-cost legacy hashes
perform additional dummy work to reach that cost's total bcrypt round count. Keep
the configured cost at least as high as existing hashes to avoid timing differences
from higher-cost legacy hashes; those hashes remain usable.

Attempts use fixed windows starting with the first attempt:

| Route | Per source | Per exact username | Across the instance |
| --- | --- | --- | --- |
| `/auth/login` | 30 / 5 minutes | 10 / 15 minutes | Bounded admission |
| `/auth/signup` | 5 / hour | 3 / hour | 100 / hour |

Source and global quotas run before binding, including invalid requests. Username
quotas run after validation and include successful and failed attempts at existing
and nonexistent accounts; success does not reset them. Both routes also share a
password-work cap of `app.maxConcurrentPasswordRequests` (default **one**), held
through lookup, bcrypt, and persistence. This cap is independent of `GOMAXPROCS`.
There is no waiting queue. Password admission saturation or an exhausted quota returns generic HTTP
429 with `Retry-After` in seconds (one second for admission saturation).

The default targets low traffic on a small server and keeps the bcrypt cost at
`14`. It is provisional until measured on deployment hardware; limiting concurrent
work does not reserve a CPU core. A canceled request retains its password slot
until its work returns because bcrypt cannot be interrupted. The shared database
request cap still applies first, so increasing the password cap above that limit
does not admit more login/signup requests. Measure login/signup alongside location
and friend requests with the deployed CPU limits before increasing concurrency.
Changing bcrypt cost needs a separate password-security decision; costs above
`14` require an explicit change to the supported ceiling and capacity review.

Sources use the socket peer address and ignore forwarding headers. IPv4-mapped
addresses share the IPv4 budget; IPv6 addresses share a `/64` budget. Behind a
reverse proxy, requests share the proxy peer's budget, so enforce client-specific
limits at the trusted gateway. Limiter storage holds at most 10,000 hashed keys,
expires entries, and rejects new keys when full without evicting active budgets.
Limits are in memory per server instance and reset on restart; multiple replicas
require coordinated limits at the gateway or a shared limiter for deployment-wide
quotas. The attempt quotas remain fixed; only password concurrency and bcrypt cost
are configurable. No database migration or index changes are required.

## Database work admission and temporary overload

One shared cap admits **four requests per server instance** across all auth and
me routes, including signup, login, refresh, friendship operations, and location
uploads. Admission runs before the protected-route session lookup and before
handler binding, bcrypt, or database calls. An admitted request holds its slot
until its authentication and handler work return. Cancellation and errors release
the slot when that work stops; a timeout does not release it prematurely while
work is still running. Existing password-work admission and quotas also remain active.

There is **no application waiting queue**. At capacity, the API immediately returns
HTTP **503 Service Unavailable**, `Retry-After: 1`, and the usual sanitized JSON
error (`code: 503`, `message: "internal error"`, `correlation_id`). The request ID
also appears in `X-Request-ID`. No database work or token rotation has started for
that rejected request. `/health`, Swagger, and routes outside those groups do not
use a database-work slot. The existing request-body size check still runs first.

All database calls for an admitted request share the same **15-second deadline**,
starting at admission and spanning session authentication, binding, password work,
and the handler. An earlier client/request deadline wins. Handlers can cancel
their own child contexts without extending that deadline. This is a database-work
budget, not a promise to interrupt bcrypt or a slow response writer at 15 seconds.
Database failures and deadlines retain the existing generic 500 response; the
503 admission response is the explicit signal that this request did not start work.

Both password and X509 MongoDB connections explicitly cap each pool at **eight**
connections. `mongo.maxPoolSize` takes precedence over `maxPoolSize` in the URI.
Other URI settings remain effective; an existing `minPoolSize` must not exceed
the configured cap. Without a URI minimum, connections open on demand. Driver
monitoring sockets are separate from this pool limit. The pool cap controls
connections, while request admission bounds callers that could wait for them;
see the [MongoDB connection-pool documentation](https://www.mongodb.com/docs/drivers/go/current/connect/connection-options/connection-pools/).

These are provisional defaults for low traffic on a small server, not measured
deployment capacity. Current friend-page reads are sequential; request admission
is not a per-query semaphore if a route adds parallel reads later. Multiple API
instances each have their own admission cap and pools and share database resources.
Measure latency, busy responses, and database/CPU usage before increasing the
limits, and budget the aggregate across replicas.

**Android retry contract:** Keep 503 separate from token expiration and token
refresh. For an API admission response, wait at least `Retry-After` seconds and
use bounded backoff with jitter, for example three total attempts with delays of
at least one and two seconds. Preserve the location fix timestamp when retrying.
A refresh rejected by admission has not consumed its credential, so it can be
retried with the same current credential after the delay. An ambiguous network
failure or another 5xx does not establish whether a write or refresh committed.
Retry only when appropriate to the operation, and keep cancellation effective.
The current Android location uploader already retries 503 with bounded 5/10-second
delays. Refresh currently treats an HTTP 503 as a generic failure rather than
retrying it; refresh handling and honoring `Retry-After` remain client follow-up
work. This server change does not modify Android code.

**Gateway/reverse proxy:** Apply a shared concurrency budget to auth and me routes
consistent with the configured API cap (four for one instance), with prompt
rejection or a small bounded queue. Keep health checks outside that budget.
Preserve 503 and `Retry-After`, and disable transparent retries of writes and
single-use refresh calls. A proxy upstream timeout should leave room for the API
deadline, for example 20 seconds for the default 15-second budget, within the
server's 30-second write timeout. Coordinate an aggregate budget when adding API
replicas. This repository does not contain a deployed gateway configuration.
No database migration or index change is required.

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

Friend lists (`GET /me/friends`) are independently paginated by `state`:
`accepted` (the default), `pending_incoming`, or `pending_outgoing`. `limit`
defaults to 50 and must be between 1 and 50. Responses are now objects rather
than the previous combined array:

```json
{"items":[{"username":"alice","status":"","state":"accepted","friend_since":null}],"next_cursor":null}
```

Pass a non-null `next_cursor` unchanged as the `cursor` query parameter for the
same authenticated account and state. A null cursor marks the final page.
Malformed parameters, invalid cursors, and cursors from another account or state
return 400 before database reads. Peer IDs define stable ascending ordering;
unchanged lists can be traversed without duplicates or omissions. Traversal is
not a snapshot across requests, so a relationship changed during loading may move
between lists. Pending entries still omit status and location. MongoDB sends at
most `limit + 1` relationship IDs and at most `limit` peer documents per page;
accepted friendship timestamps are projected only for those IDs.

Each account may have **2,048 accepted friends**, **256 incoming requests**, and
**256 outgoing requests**. Sending checks both users' accepted capacity, the
sender's outgoing capacity, and the recipient's incoming capacity. Acceptance
checks both users' accepted capacity. Exceeding a limit returns 409. Repeating
an existing pending request is a no-op even at the pending limit. A failed
acceptance preserves the request and both users' friend lists. Accepting clears
any reverse pending request; rejecting, removing, and accepting requests free
their pending slots. Removing a friend frees both accepted slots.

These mutations run in MongoDB transactions and serialize on both user documents
with an internal `relationships_version` field, preventing concurrent or
multi-server calls from exceeding a cap. The field is created lazily; no data
migration or counters are required. Existing over-limit lists are retained and
remain pageable, but cannot grow further until below the limit. The added
`{to: 1, from: 1}` index supports bounded, sorted incoming-request pages; server
startup and the `mongoIndexes` CLI create it. The existing unique `{from: 1, to: 1}`
index supports outgoing pages.

**Rollout:** Deploy the coordinated Android update with this response change.
Android loads every page in each state before publishing or replacing its cache;
any page failure preserves the cache. It deduplicates users whose relationships
move while loading and handles 409 with a capacity message. Older Android versions
expect an array and require an update before using this server version.

The server now requires **MongoDB 6+ on a replica set or sharded cluster** for
pagination and atomic relationship updates. A standalone MongoDB server cannot
run these transactions. Convert an existing standalone deployment to a replica
set before rollout, retaining its volume and data. Compose configures a primary
named `rs0`; add a privately generated `MONGODB_REPLICA_SET_KEY` to ignored
`secrets.env` alongside the existing MongoDB credentials. Ensure the application
URI selects the replica set and its advertised host is reachable. For an existing
volume, verify that the replica set is initialized and writable before starting
the API; never delete the volume to change topology. Atlas replica sets already
support transactions. Test containers should also run with `--replSet rs0` and
be initialized before the regression suite.

Location uploads (`PUT /me/location`) require `last_update` as a positive integer
in Unix milliseconds (UTC), representing when the fix was measured. The server
rejects missing, null, malformed, and out-of-range timestamps with 400 before
writing to the database. The accepted window is from 24 hours before server receipt
time through 5 minutes after it, inclusive. These limits allow delayed uploads and
modest device clock skew; they are application policy, not a universal standard.
Accepted future timestamps are capped at server receipt time, at millisecond precision.
MongoDB atomically replaces a location only when the incoming fix is newer. Older
and duplicate fixes return 204 without changing the stored location, making retries
safe under concurrent uploads. New users and wiped locations accept their first
valid fix. Android clients should send the location provider's fix time, preserve
it on retry, and treat 400 as a permanent rejection of that fix.

Docs are served in /swagger endpoint.
Ref: https://github.com/swaggo/echo-swagger

For generating docs (required each time something is changed) `swag init -g cmd/server/main.go`
and commit it to the repository.

## Binding Requests

All request bodies are limited to 16 KiB before authentication and binding,
including chunked bodies and trailing data. Oversized requests return HTTP 413.
Routes documented with a JSON body require `Content-Type: application/json`
(optional media type parameters such as `charset=utf-8` are accepted); other or
missing content types return HTTP 415. Routes without a documented body do not
require a content type.

Usernames are limited to 64 Unicode characters, device identifiers to 256, and
statuses to 1,024. An empty status clears it. Passwords are limited to 72 bytes
(the bcrypt limit), and signup still requires at least eight characters.
Oversized fields return HTTP 400. Encoded access and refresh JWTs are limited to
4,096 bytes before parsing; oversized Bearer credentials return HTTP 403.
These validation limits apply to new requests; existing stored fields are not
rewritten.

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
