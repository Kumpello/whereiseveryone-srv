package jsonerr

import (
	"encoding/json"
	"fmt"

	"github.com/labstack/echo/v4"
)

type JSONError struct {
	// Message is human friendly error message
	Message string `json:"message"`
	// Code is desired http code for this error
	Code int `json:"code"`
	// Err is a golang error returned by the app
	// It is removed in production application (TBD)
	Err error `json:"error" swaggertype:"string"`
}

func (h JSONError) MarshalJSON() ([]byte, error) {
	var errMsg string
	if h.Err != nil {
		errMsg = h.Err.Error()
	}

	return json.Marshal(&struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
		Error   string `json:"error"`
	}{
		Message: h.Message,
		Code:    h.Code,
		Error:   errMsg,
	})
}

func (h JSONError) Error() string {
	if h.Err != nil {
		return fmt.Sprintf("%s: %s", h.Message, h.Err)
	}

	return h.Message
}

func (h JSONError) Echo(context echo.Context) error {
	return context.JSON(h.Code, h)
}

func EchoError(code int, message string, err error) *JSONError {
	httpErr := JSONError{message, code, err}
	return &httpErr
}

func EchoInvalidRequestError(err error) *JSONError {
	return EchoError(400, "invalid request", err)
}

func EchoInvalidTokenError(err error) *JSONError {
	return EchoError(401, "expired token", err)
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
