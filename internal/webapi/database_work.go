package webapi

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v5"

	"whereiseveryone/internal/config"
	"whereiseveryone/internal/webapi/jsonerr"
)

// DatabaseRequestContext preserves the admission deadline while giving each
// handler its own cancellation. Without a parent deadline, it uses the default budget.
func DatabaseRequestContext(parent context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := parent.Deadline(); hasDeadline {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, config.DefaultDBRequestTimeout)
}

func databaseWorkAdmission(limits config.DatabaseRequestLimits) echo.MiddlewareFunc {
	slots := make(chan struct{}, limits.MaxRequests)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			parent := c.Request().Context()
			if err := parent.Err(); err != nil {
				return jsonerr.EchoInternalError(err).Echo(c)
			}
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				c.Response().Header().Set("Retry-After", "1")
				return jsonerr.EchoError(http.StatusServiceUnavailable, "internal error", nil).Echo(c)
			}
			ctx, cancel := context.WithTimeout(parent, limits.Timeout)
			defer cancel()
			if err := ctx.Err(); err != nil {
				return jsonerr.EchoInternalError(err).Echo(c)
			}
			c.SetRequest(c.Request().WithContext(ctx))
			// Keep the slot until work actually returns, including after cancellation.
			return next(c)
		}
	}
}
