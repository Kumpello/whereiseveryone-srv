package binder

import (
	"context"
	"reflect"
	"whereiseveryone/internal/webapi"
	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"

	"github.com/labstack/echo/v5"
)

// BaseContext is interface over Context without generic type
// Allows to use Context without generic type
type BaseContext interface {
	Context() context.Context
	Cancel()
	Echo() *echo.Context
	UserID() id.ID
	TokenData() jwt.SignedToken
}

type EmptyBody struct {
}

type Context[T any] struct {
	ctx    context.Context
	cancel context.CancelFunc
	echo   *echo.Context

	userID    id.ID
	tokenData jwt.SignedToken

	Request T
}

func (c Context[T]) Context() context.Context {
	return c.ctx
}

// Cancel releases the request context and its timeout resources. It is safe to
// call repeatedly and should be deferred by the caller after successful binding.
func (c Context[T]) Cancel() {
	c.cancel()
}

// Echo returns the underlying HTTP request context.
func (c Context[T]) Echo() *echo.Context {
	return c.echo
}

func (c Context[T]) UserID() id.ID {
	return c.userID
}

func (c Context[T]) TokenData() jwt.SignedToken {
	return c.tokenData
}

type StructValidator interface {
	Struct(str any) error
}

// BindRequest bind requests returning Context, user data (if requireAuth) and an error.
// T must be a simple type to be validated (pointers are not validated).
// Binder returns an jsonerr.JSONError but it doesn't bind the error.
// On failure, the returned context is already canceled. On success, the caller
// owns cancellation and should defer result.Cancel().
func BindRequest[T any](
	c *echo.Context,
	requireAuth bool,
) (*Context[T], *jsonerr.JSONError) {
	result := &Context[T]{
		echo: c,
	}
	var t T

	// Obtain context and cancel
	reqCtx, cancel := webapi.DatabaseRequestContext(c.Request().Context())
	result.ctx = reqCtx
	result.cancel = cancel

	if requireAuth {
		jwtToken, err := webapi.GetJWTToken(c)
		if err != nil {
			cancel()
			c.Logger().Error("Failed to get JWT token", "error", err)
			return result, webapi.JWTErrorToJSONError(err)
		}
		requesterID, err := id.FromString(jwtToken.ID)
		if err != nil {
			cancel()
			c.Logger().Error("Failed to get requester ID", "error", err)
			return result, jsonerr.EchoInvalidRequestError(err)
		}
		result.userID = requesterID
		result.tokenData = jwtToken
	}

	// Obtain request
	if err := c.Bind(&t); err != nil {
		cancel()
		c.Logger().Error("Failed to bind request", "error", err)
		return result, jsonerr.EchoInvalidRequestError(err)
	}

	if val := reflect.ValueOf(t); val.Kind() == reflect.Struct { // don't validate interface{} type
		if err := c.Validate(t); err != nil {
			cancel()
			c.Logger().Error("Failed to validate request", "error", err)
			return result, jsonerr.EchoInvalidRequestError(err)
		}
	}

	result.Request = t
	return result, nil
}
