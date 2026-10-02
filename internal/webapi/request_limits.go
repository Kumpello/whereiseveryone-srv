package webapi

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/labstack/echo/v5"
	"whereiseveryone/internal/webapi/jsonerr"
)

// MaxRequestBodyBytes bounds all HTTP request bodies before binding or authentication.
const MaxRequestBodyBytes = 16 * 1024

func limitRequestBody(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		req := c.Request()
		if req.ContentLength > MaxRequestBodyBytes {
			return jsonerr.EchoError(http.StatusRequestEntityTooLarge, "request body too large", nil).Echo(c)
		}
		// Read the complete body through a byte limit. This also covers unknown
		// lengths and oversized trailing data that a JSON decoder may never read.
		body, err := io.ReadAll(http.MaxBytesReader(c.Response(), req.Body, MaxRequestBodyBytes))
		if err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				return jsonerr.EchoError(http.StatusRequestEntityTooLarge, "request body too large", nil).Echo(c)
			}
			return jsonerr.EchoInvalidRequestError(err).Echo(c)
		}
		req.Body = struct {
			io.Reader
			io.Closer
		}{bytes.NewReader(body), req.Body}
		req.ContentLength = int64(len(body))
		return next(c)
	}
}

// RequireJSON restricts routes with a documented JSON body to application/json.
func RequireJSON(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		mediaType, _, err := mime.ParseMediaType(c.Request().Header.Get(echo.HeaderContentType))
		if err != nil || mediaType != echo.MIMEApplicationJSON {
			return jsonerr.EchoError(http.StatusUnsupportedMediaType, "content type must be application/json", nil).Echo(c)
		}
		// Echo's default binder compares media types case-sensitively.
		c.Request().Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		return next(c)
	}
}
