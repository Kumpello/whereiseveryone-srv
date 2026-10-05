# Go for a Kotlin developer, using this repository

You already know many of the architectural ideas in this backend: dependency injection, interfaces, repositories, DTOs, and cancellation. The main adjustment is how Go expresses them. Go uses structs and methods instead of classes, returns errors explicitly, and makes concurrency and resource cleanup visible in ordinary code.

This guide describes the source inspected on October 1, 2026. Kotlin examples are conceptual comparisons, not exact translations. Shortened Go examples are marked as such. Some existing code has pitfalls; those are called out rather than presented as recommendations.

**Start with the project map.**

| Location | Purpose | Familiar Android comparison |
| --- | --- | --- |
| `go.mod` | Module import path, Go version, dependencies | Parts of Gradle configuration |
| `go.sum` | Dependency integrity checksums | Dependency verification metadata; not a lockfile by itself |
| `cmd/server/main.go` | Constructs dependencies and starts HTTP server | Application startup and manual DI setup |
| `cmd/cli/` | A separate administrative executable | A separate command-line entry point |
| `internal/webapi/` | Echo routing, authentication, request/response handling | HTTP boundary; broadly a controller layer |
| `internal/users/` | User models, persistence interfaces, MongoDB implementations | Repository contracts and data sources |
| `internal/config/` | Configuration keys and secret provisioning/validation | Runtime configuration |
| `pkg/` | Utilities such as clocks, IDs, JWTs, and configuration readers | Shared utility packages |
| `*_test.go` | Tests alongside the package being tested | Unit tests, without a separate `src/test` tree |
| `docs/` | Swagger artifacts and this guide | API documentation |

This is not an Android MVVM application. It has no UI state or ViewModel layer. A typical path is HTTP handler → persistence adapter → MongoDB. Some application decisions live directly in handlers; there is no mandatory use-case class between every layer.

**Packages and visibility replace some Kotlin conventions.**

Go normally groups source files by directory into a package. Functions and types in different files of the same package can refer to each other without imports. Imports use paths such as `whereiseveryone/internal/users`; `whereiseveryone` comes from `go.mod`.

Capitalization controls visibility across packages:

```go
type User struct { /* fields */ } // exported
type mux struct { /* fields */ } // package-private

func NewMux(/* parameters */) *mux { /* implementation */ }
func (m *mux) updateStatus(/* parameters */) error { /* implementation */ }
```

These signatures are shortened from [the me router](../internal/webapi/me/mux.go). `NewMux` is public, while its returned concrete type is private. Another package can still call the constructor, keep its result using type inference, and use its exported methods.

Lowercase means package-private, not file-private or Kotlin class-private. `internal/` also has compiler-enforced import restrictions: code outside the allowed parent tree cannot import those packages. `pkg/` and `cmd/` are naming conventions, not visibility keywords.

`package main` with `func main()` defines an executable entry point. The server and CLI are separate programs in the same module.

**Learn to read declarations before the architecture.**

```go
status := "Available"
var attempts int
const defaultPort = "8080"
```

`:=` declares local variables with inferred types. It is not Kotlin `val`: `status` can subsequently be reassigned. `var attempts int` initializes `attempts` to zero. `const` supports compile-time constants, not arbitrary immutable objects.

Go always initializes variables. Common zero values are `0`, `false`, `""`, and `nil` for pointers, slices, maps, interfaces, channels, and functions. A struct's zero value recursively initializes its fields. There is no `lateinit` needed for ordinary declarations.

Inside one scope, `:=` must introduce at least one new non-blank variable. Thus `user, err := ...` can reuse an existing `err` when `user` is new. In a nested scope, the same spelling can declare a different variable and shadow the outer one.

**Structs and receiver methods are the nearest equivalent to classes.**

From [the me router](../internal/webapi/me/mux.go):

```go
type mux struct {
	userAdapter users.Adapter
	timer       timer.Timer
}

func NewMux(userAdapter users.Adapter, timer timer.Timer) *mux {
	return &mux{userAdapter: userAdapter, timer: timer}
}
```

Conceptually:

```kotlin
class MeController(
    private val userAdapter: UserRepository,
    private val timer: Clock,
)
```

`NewMux` is an ordinary function, not a language-level constructor. The `New...` prefix is a convention. A struct does not automatically provide Kotlin data-class features such as `copy()` or destructuring.

In `func (m *mux) updateStatus(c echo.Context) error`, `(m *mux)` is the **receiver**. It attaches the method to `mux`; `m` plays the role of an explicitly named `this`. `c echo.Context` is the parameter and `error` is the return type.

**Pointers explain both shared mutation and optional values.**

Go distinguishes a value `User` from a pointer `*User`:

```go
user := users.User{Status: "Available"}
pointer := &user
pointer.Status = "Busy"
```

`&user` obtains a pointer; `*pointer` dereferences it. Field access automatically dereferences pointers, so `pointer.Status` works. Go has garbage collection: returning a pointer to a local variable is safe, and you do not manually free it.

A value receiver, such as `(u User)`, receives a copy. A pointer receiver, such as `(m *mux)`, receives a pointer to the original object. However, copying a struct is shallow: maps, slices, and pointers inside the copy can still refer to shared data. A value receiver does not guarantee immutability or thread safety.

In [the user model](../internal/users/user.go), `Location *Location` represents a potentially absent location. This resembles Kotlin `Location?`, but Go does not have Kotlin's compiler-enforced null-safety system. Dereferencing a nil pointer panics.

Pointers can also encode an update instruction. In [UpdateTokens](../internal/users/auth.go), a `*string` parameter means:

| Value | Meaning in this method |
| --- | --- |
| `nil` | Leave the stored field unchanged |
| Pointer to `""` | Set the stored field to an empty string |
| Pointer to a nonempty string | Replace the stored field |

That distinction is part of the method's contract, not a built-in meaning of pointers.

**Interfaces are implemented implicitly.**

[The timer abstraction](../pkg/timer/timer.go) is a small example:

```go
type Timer interface {
	Now() time.Time
}

type UTCTimer struct{}

func (s UTCTimer) Now() time.Time {
	return time.Now().UTC()
}
```

Kotlin would require something like `class UtcTimer : Timer` and `override fun now()`. Go requires neither declaration: a type implements an interface by having the required methods with matching signatures.

`struct{}` is an empty struct, useful when an implementation needs behavior but no fields. The receiver above is a value receiver, so both `UTCTimer` and `*UTCTimer` satisfy `Timer`. If the required method existed only on `*UTCTimer`, the non-pointer value would not satisfy that interface.

The tests supply another implementation:

```go
type fixedTimer struct{ now time.Time }

func (c fixedTimer) Now() time.Time { return c.now }
```

It automatically satisfies the same interface. [JWT tests](../pkg/jwt/tokens_test.go) can move the clock or fix it at a known instant without sleeping.

You will also see:

```go
var _ locationAdapter = (*mongoLocationAdapter)(nil)
```

In [location.go](../internal/users/location.go), this is a compile-time interface check. It verifies that the pointer type implements the contract. `_` discards the value; this line is not creating a real database adapter.

**Dependency injection is ordinary function calls.**

[Server startup](../cmd/server/main.go) creates a logger, clock, MongoDB collections, user adapter, JWT service, and routers. It then passes them into constructors. This is the same architectural idea as constructor injection with Hilt, but there is no annotation processor or DI container.

The application depends on abstractions where substitution is useful:

- `timer.Timer` allows real and fake clocks.
- `env.Handler` allows JSON-file and OS-environment configuration.
- `users.Adapter` separates handlers from MongoDB implementation details.
- `webapi.SessionReader` exposes `GetSession`, which reads only the credentials
  needed by authentication. Full user reads remain available to other handlers.

`SessionReader` is a particularly useful pattern: the consumer asks for the small interface it needs. The same Mongo adapter can satisfy both a broad persistence interface and this smaller interface.

The type called `Adapter` here largely fills the role of an Android repository/data-source boundary. Pattern names are less important than which dependency is being hidden.

**Embedding composes behavior; it is not class inheritance.**

In [user.go](../internal/users/user.go), the main adapter embeds smaller interfaces:

```go
type mongoUserAdapter struct {
	locationAdapter
	authAdapter
	pendingFriendRequestAdapter

	// Other fields omitted.
}
```

An embedded field has a type but no separately written field name. Its methods are promoted, so a caller can invoke `adapter.UpdateLocation(...)` through the outer adapter. The constructor supplies a `mongoLocationAdapter` to implement that part.

This resembles Kotlin interface delegation (`LocationRepository by locationRepository`), though the language mechanics differ. There is no superclass constructor or virtual-method inheritance hierarchy. Embedded interface fields must still be initialized; their zero value is nil.

The CLI uses the same composition mechanism by embedding `*cobra.Command` in `commandApp`.

**Errors are returned values, not thrown exceptions.**

A common signature is:

```go
func (m *mongoUserAdapter) GetUser(ctx context.Context, userID id.ID) (User, error)
```

The function returns two separate values. Typical calling code is:

```go
user, err := adapter.GetUser(ctx, userID)
if err != nil {
	return err
}
// Use user after checking err.
```

This is loosely comparable to branching on Kotlin `Result<User>`, but Go does not encode success/failure alternatives as a sealed union here. The usual contract is “use the result when `err == nil`.”

`error` is an interface with one method, `Error() string`. [JSONError](../internal/webapi/jsonerr/error.go) implements it simply by providing that method.

Database code often wraps an error:

```go
return User{}, fmt.Errorf("find user by id: %w", err)
```

`User{}` is the zero-value user. `%w` preserves the underlying error while adding context. `errors.Is(err, target)` checks through wrapping; `errors.As(err, &typedError)` finds an error of a particular type. Compare with checking an exception's cause/type in Kotlin. Avoid comparing error-message strings.

`panic`/`recover` exist, but ordinary validation and database failures should use returned errors. Also, process termination through `log.Fatal` does not run deferred cleanup; it is not equivalent to returning an error from a handler.

**There are two different “contexts” in the HTTP code.**

`echo.Context` provides request/response operations: binding JSON, reading headers, and writing responses. `context.Context` carries cancellation, deadlines, and request-scoped values through work such as database calls. Neither is Android's `Context`.

In [the binder](../internal/webapi/binder/binder.go):

```go
reqCtx, cancel := context.WithTimeout(c.Request().Context(), requestTimeout)
```

This derives a deadline from the HTTP request's context. The binder currently uses 15 seconds. Mongo operations receive that derived context so they can stop when the deadline expires or the request is canceled.

The closest Kotlin analogy is cancellation/deadlines propagated through a coroutine job. Go cancellation is cooperative: passing a context does not forcibly interrupt arbitrary computation, and creating a context does not launch a goroutine. Do not put an `echo.Context` into background work that outlives the handler; Echo reuses these objects.

**`defer` schedules a function call when the surrounding function exits.**

```go
ctx, cancel := context.WithTimeout(parent, 15*time.Second)
defer cancel()
```

Think of a function-wide `try/finally` or Kotlin `use` for resource cleanup. Deferred calls execute on ordinary return and during panic unwinding, in reverse registration order. They run at function exit, not the end of the nearest `if` or loop block. The function and arguments for the deferred call are evaluated when the `defer` statement executes.

The Mongo query code uses `defer c.Close(ctx)` to close a cursor, even when decoding returns an error.

The binder exposes cancellation as an action:

```go
func (c Context[T]) Cancel() {
	c.cancel()
}
```

After successful binding, the handler takes responsibility for cleanup:

```go
defer request.Cancel()
```

An earlier version returned `context.CancelFunc` from `Cancel()` instead of calling it. With that API, `defer request.Cancel()` only deferred a getter and discarded the cancellation function it returned. The method now performs cancellation directly. The binder cancels its context before returning any binding error, so handlers can safely return on errors before registering their own deferred cleanup. Successful binding leaves the context active until the caller cancels it, its parent is canceled, or its deadline expires.

**Trace one request: `PUT /me/status`.**

Imagine the Android client sends an authenticated request with `{"status":"On my way"}`:

```text
Android HTTP client
    → Echo authentication middleware
    → me.updateStatus
    → binder.BindRequest[updateStatusRequest]
    → users.Adapter.UpdateStatus
    → MongoDB UpdateOne
    → HTTP 204 back to Android
```

1. [NewEcho](../internal/webapi/init.go) attaches authentication middleware to the `/me` group. It verifies an access JWT, loads the user's stored session, compares tokens, and stores the validated claims in Echo's request context. This repository currently uses persisted session checks as well as JWT verification.
2. [Route](../internal/webapi/me/mux.go) registers `g.PUT("/status", m.updateStatus)`. The `/me` prefix comes from the parent group.
3. `BindRequest[updateStatusRequest](c, true)` decodes and validates the DTO, derives a timeout, and reads the authenticated user ID. The target user comes from authenticated claims, not a user ID supplied in this request body.
4. The handler calls `m.userAdapter.UpdateStatus(request.Context(), request.UserID(), status)`.
5. [The Mongo implementation](../internal/users/user.go) filters by `_id` and uses `$set` to update the status.
6. The handler returns `c.NoContent(204)`, or converts an error into an HTTP response.

An Echo handler returns `error`. Returning `c.JSON(...)` or `c.NoContent(...)` means “write the response and return any response-writing error,” not “return the response object.”

**Generics make binding reusable while preserving DTO types.**

The binder's signature is:

```go
func BindRequest[T any](
	c echo.Context,
	requireAuth bool,
) (*Context[T], *jsonerr.JSONError)
```

Read `[T any]` like a Kotlin generic type parameter with no restrictive constraint. `any` is an alias for the empty interface. Calling `BindRequest[updateStatusRequest]` makes `request.Request` an `updateStatusRequest`, so its fields are statically known.

The square brackets specify a type argument; parentheses supply ordinary arguments. `var t T` creates the type's zero value and `c.Bind(&t)` passes its address so decoding can fill it.

This repository also uses reflection to decide whether to run struct validation. Generics provide the type relationship; reflection inspects a runtime value. Those are different mechanisms.

**DTO tags and custom serializers define the Android-facing contract.**

From [the response types](../internal/webapi/me/types.go):

```go
type friendDetails struct {
	Username    string           `json:"username"`
	Status      string           `json:"status"`
	State       friendState      `json:"state"`
	Location    *locationDetails `json:"location,omitempty"`
	FriendSince *timestamp       `json:"friend_since"`
}
```

Struct tags are metadata read by libraries, roughly comparable to `@SerialName`, `@Json`, or validation annotations. `json` controls HTTP serialization; `bson` controls MongoDB encoding; `validate` is read by the validator library. A tag does not execute validation by itself.

An unexported struct type can still serialize exported fields such as `Username`. Ordinary unexported fields are not encoded by the standard JSON encoder.

For these pointer fields, `Location == nil` is omitted because of `omitempty`, while `FriendSince == nil` produces `"friend_since": null`. Numeric fields with `omitempty` also omit zero, which matters when zero is meaningful, for example speed.

`type timestamp time.Time` creates a **new defined type**. Its `MarshalJSON` and `UnmarshalJSON` methods encode/decode Unix milliseconds, analogous to a custom kotlinx.serialization serializer. The new type does not automatically inherit all `time.Time` methods, so the code explicitly converts with `time.Time(t)`.

By contrast, `type ID = primitive.ObjectID` in [id.go](../pkg/id/id.go) is an **alias**, like Kotlin `typealias`; it introduces no distinct type. Likewise, `type Key string` is a distinct defined type, not an alias. `friendState` is a string-based defined type with constants, not an exhaustive Kotlin enum or sealed class.

The API builds `friendDetails` from database users rather than serializing whole user records. That is the familiar entity-to-DTO mapping pattern, and it keeps authentication fields out of friend responses.

**Slices and maps are worth learning early.**

`[]users.User` is a slice: a view over an underlying array with a length and capacity. It is the usual Go collection for list-like data. `[32]byte` is a fixed-size array whose length is part of its type.

```go
users := make([]User, 0, len(ids))
users = append(users, user)
```

This creates a slice with length zero and preallocated capacity. `append` returns an updated slice, which is why it is assigned back. Copying a slice does not copy its elements; slices can share the underlying array. This differs from assuming every assignment creates an independent list.

In `append(friends, load.friends...)`, `...` expands a slice into variadic arguments, similar to spreading an array into a Kotlin `vararg` call.

`map[string]time.Time` resembles a mutable map. The lookup `value, ok := m[key]` returns both the value and whether the key exists; a missing entry otherwise gives the value type's zero value. Reading a nil map is allowed; writing one requires initialization, such as `make(map[string]time.Time)`.

Nil and empty slices both have length zero, but default JSON serialization distinguishes `null` from `[]`. This repo deliberately constructs empty result slices in places where an array response is desired.

**Goroutines and channels handle concurrent work.**

In [getFriends](../internal/webapi/me/mux.go), three independent branches load accepted friends, incoming requests, and outgoing requests. A shortened example:

```go
loads := make(chan friendsLoad, 3)

go func() {
	friends, err := m.userAdapter.GetUsers(ctx, user.SubscribedUsers)
	loads <- friendsLoad{friends: friends, err: err}
}()

// Two additional workers are launched in the actual handler.
for range 3 {
	load := <-loads
	// Check the error and collect results.
}
```

`go` starts a goroutine: a lightweight concurrent execution managed by Go's runtime. `func() { ... }` is an anonymous function, and the final `()` invokes it. `<-` sends or receives channel values depending on its position. `make(chan friendsLoad, 3)` creates a channel with three buffered slots. `for range 3` repeats three times.

This resembles Kotlin `async` for independent loads followed by awaiting results, but plain goroutines do not automatically form a structured coroutine scope. The programmer must arrange cancellation, result collection, and lifetime management. Returning from the handler does not automatically join its workers.

Here each of the three workers sends one result, and the buffer can hold all three. Even if the handler returns on an early error, the other workers can finish their send. The workers receive `ctx` for cancellation, and the handler's deferred `request.Cancel()` signals cancellation when it returns. There is no need to close this channel for a fixed count of receives; closing channels is not routine resource cleanup.

HTTP handlers already run concurrently. Shared router fields such as adapters and the password semaphore are used by multiple requests. Do not casually put per-request mutable state in `mux`, or assume a pointer makes shared state safe. Channels, mutexes, or ownership rules are needed when mutable data is shared.

**A channel can also act as a semaphore.**

[The authentication router](../internal/webapi/auth/mux.go) creates a buffered `chan struct{}` named `passwordOps`. Sending an empty struct acquires a slot; receiving releases it. `acquirePasswordSlot` uses `select` to wait for either a slot or context cancellation and returns a release function.

This is comparable to Kotlin's `Semaphore.withPermit` for limiting concurrent expensive password work. It limits simultaneous operations, not the number of login attempts over time. `select` waits for a ready channel operation; it is not an ordinary value-based `switch`.

**Middleware is a function wrapping another function.**

Echo middleware has the shape:

```go
func(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		// Work before the handler, or reject the request here.
		return next(c)
	}
}
```

The pattern resembles an OkHttp interceptor: authentication can stop the chain, while logging can measure work around `next(c)`. It also demonstrates Go closures: the returned function captures dependencies such as the JWT service and session reader.

In `GetJWTToken`, `token.(jwt.SignedToken)` is a type assertion. The two-result form `jwtToken, ok := ...` reports whether the assertion succeeded instead of panicking, loosely like a safe Kotlin cast followed by checking its result.

**Testing uses normal Go functions and small fakes.**

Tests are named `TestXxx`, accept `*testing.T`, and live in `*_test.go` files. `t.Fatal`/`t.Fatalf` fail the current test immediately; `t.Run` creates a named subtest. The token tests use a slice of cases and iterate over it—the common **table-driven test** pattern, similar to parameterized JUnit tests.

[Session tests](../internal/webapi/session_test.go) use `httptest.NewRequest`, `httptest.NewRecorder`, and `ServeHTTP` to run requests through actual middleware and routing without opening a TCP port. An in-memory adapter substitutes for MongoDB. This gives more coverage than invoking a handler function in isolation while staying independent of an external database.

Tests declared `package webapi_test` exercise the package through exported APIs. Tests declared `package jwt` can also access package-private code. Both styles are useful.

**Use a small set of commands while learning.**

Run these from the repository root with the project's Go toolchain:

```sh
go build ./...
go test ./...
go test -v ./pkg/jwt
go test -run TestTimestampJSONRoundTrip -v ./internal/webapi/me
gofmt -w path/to/changed_file.go
```

`./...` selects packages recursively. `go build` compiles; `go test` runs tests; `go run ./cmd/server` compiles and runs an executable. `gofmt` handles standard Go formatting. `go test -race ./...` is useful when changing concurrency and your platform supports the race detector; it finds races exercised by those tests, not every possible race.

To launch the API, first generate a private configuration with `go run ./cmd/cli initConfig`, fill in database settings, and run MongoDB. Then use `go run ./cmd/server --config=./.env/local.json`. The generator refuses to overwrite an existing file. The server needs real configuration and a reachable database; ordinary unit tests do not.

For a first reading session, follow `pkg/timer/timer.go` → `pkg/env/env.go` and `json.go` → `internal/webapi/me/mux.go` (`updateStatus`) → `internal/webapi/binder/binder.go` → `internal/users/user.go` (`UpdateStatus`). Then read `getFriends` for concurrency and `pkg/jwt/tokens_test.go` for testing patterns. That route introduces the language through one working feature before the more involved authentication flow.
