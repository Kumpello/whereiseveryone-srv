// Package jsonerr separates public HTTP errors from private diagnostic details.
package jsonerr

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v5"
)

// JSONError carries a public response and an error reserved for server logs.
type JSONError struct {
	// Message is human friendly error message
	Message string `json:"message"`
	// Code is desired http code for this error
	Code int `json:"code"`
	// CorrelationID identifies the request in server logs and the X-Request-ID header.
	CorrelationID string `json:"correlation_id"`
	// Err is retained only for server-side logging, never for JSON serialization.
	Err error `json:"-" swaggerignore:"true"`
}

const (
	correlationKey  = "jsonerr.correlation_id"
	requestErrorKey = "jsonerr.request_error"
)

// CorrelationID creates a server-generated identifier once per request.
func CorrelationID(c *echo.Context) string {
	value, ok := c.Get(correlationKey).(string)
	if !ok || value == "" {
		value = rand.Text()
		c.Set(correlationKey, value)
	}
	c.Response().Header().Set(echo.HeaderXRequestID, value)
	return value
}

// RequestError retrieves errors from handlers that wrote their response directly.
func RequestError(c *echo.Context) error {
	err, _ := c.Get(requestErrorKey).(error)
	return err
}

// MarshalJSON serializes only public fields and sanitizes server error messages.
func (h JSONError) MarshalJSON() ([]byte, error) {
	message := h.Message
	if h.Code >= http.StatusInternalServerError {
		message = "internal error"
	}
	encoded, err := json.Marshal(struct {
		Message       string `json:"message"`
		Code          int    `json:"code"`
		CorrelationID string `json:"correlation_id"`
	}{Message: message, Code: h.Code, CorrelationID: h.CorrelationID})
	if err != nil {
		return nil, fmt.Errorf("marshal public error: %w", err)
	}
	return encoded, nil
}

// StatusCode allows the global handler to preserve an error's HTTP status.
func (h JSONError) StatusCode() int { return h.Code }

func (h JSONError) Error() string {
	if h.Err != nil {
		return fmt.Sprintf("%s: %s", h.Message, h.Err)
	}

	return h.Message
}

// Echo writes the JSON error response with its HTTP status.
func (h JSONError) Echo(context *echo.Context) error {
	h.CorrelationID = CorrelationID(context)
	if h.Err != nil {
		context.Set(requestErrorKey, h)
	}
	if context.Request().Method == http.MethodHead {
		return context.NoContent(h.Code)
	}
	return context.JSON(h.Code, h)
}

func EchoError(code int, message string, err error) *JSONError {
	httpErr := JSONError{Message: message, Code: code, Err: err}
	return &httpErr
}

func EchoInvalidRequestError(err error) *JSONError {
	return EchoError(400, "invalid request", err)
}

func EchoExpiredTokenError() *JSONError {
	return EchoError(401, "expired token", nil)
}

func EchoNotFoundError(err error) *JSONError {
	return EchoError(404, "not found", err)
}

func EchoInternalError(err error) *JSONError {
	return EchoError(500, "internal error", err)
}

func EchoForbiddenError() *JSONError {
	return EchoError(403, "forbidden", nil)
}

func EchoConflictError(err error) *JSONError {
	return EchoError(409, "conflict", err)
}
