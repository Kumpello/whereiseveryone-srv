// Package webapi configures HTTP routing, authentication, and public responses.
package webapi

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/logger"
)

func requestCorrelation(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		jsonerr.CorrelationID(c)
		return next(c)
	}
}

// Framework errors can contain arbitrary messages or custom JSON marshalers.
// Only their HTTP status is public; their details belong in the request log.
func publicHTTPErrorHandler(c *echo.Context, err error) {
	response, unwrapErr := echo.UnwrapResponse(c.Response())
	if unwrapErr == nil && response != nil && response.Committed {
		return
	}
	code := http.StatusInternalServerError
	var statusError echo.HTTPStatusCoder
	if errors.As(err, &statusError) {
		if status := statusError.StatusCode(); status >= 400 && status <= 599 {
			code = status
		}
	}
	if writeErr := jsonerr.EchoError(code, http.StatusText(code), err).Echo(c); writeErr != nil {
		c.Logger().Error("failed to send public error", "correlation_id", jsonerr.CorrelationID(c), "error", writeErr)
	}
}

func requestLogger(log logger.Logger) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		HandleError: true,
		LogStatus:   true,
		LogLatency:  true,
		LogValuesFunc: func(c *echo.Context, values middleware.RequestLoggerValues) error {
			entry := logger.MakeEchoLogEntry(log, c).
				WithField("status", values.Status).
				WithField("latency", values.Latency.String())
			err := values.Error
			if recorded := jsonerr.RequestError(c); recorded != nil {
				err = recorded
			}
			switch {
			case err != nil:
				entry.WithError(err).Warn("request failed")
			case values.Status >= http.StatusBadRequest:
				entry.Warn("request failed")
			default:
				entry.Info("request completed")
			}
			return nil
		},
	})
}
