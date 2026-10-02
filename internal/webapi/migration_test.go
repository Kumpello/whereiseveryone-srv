package webapi_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"
	"github.com/sirupsen/logrus"
	echoSwagger "github.com/swaggo/echo-swagger/v2"
	_ "whereiseveryone/docs"
	"whereiseveryone/internal/webapi"
)

type migrationRouter struct{}

func (migrationRouter) Route(g *echo.Group, _ echo.MiddlewareFunc) {
	g.GET("/error", func(_ *echo.Context) error { return errors.New("private failure details") })
}

func TestEchoRoutingSwaggerAndErrorLogging(t *testing.T) {
	var logs bytes.Buffer
	log := logrus.New()
	log.SetOutput(&logs)
	log.SetFormatter(&logrus.JSONFormatter{})
	e := webapi.NewEcho("", validator.New(), nil, nil, webapi.EchoRouters{
		Swagger: echoSwagger.WrapHandler, AuthRouter: migrationRouter{}, MeRouter: migrationRouter{},
	}, log, false)
	for _, tc := range []struct {
		method, path string
		status       int
		failed       bool
	}{
		{http.MethodGet, "/health", 200, false},
		{http.MethodGet, "/missing", 404, true},
		{http.MethodPost, "/health", 405, true},
		{http.MethodGet, "/auth/error", 500, true},
		{http.MethodGet, "/swagger/index.html", 200, false},
		{http.MethodGet, "/swagger/doc.json", 200, false},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			logs.Reset()
			response := httptest.NewRecorder()
			e.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil))
			if response.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "private failure details") {
				t.Fatal("internal error leaked to client")
			}
			var entry struct {
				Status  int    `json:"status"`
				Message string `json:"msg"`
				Level   string `json:"level"`
			}
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("expected one structured request log: %v: %s", err, logs.String())
			}
			wantMessage, wantLevel := "request completed", "info"
			if tc.failed {
				wantMessage, wantLevel = "request failed", "warning"
			}
			if entry.Status != tc.status || entry.Message != wantMessage || entry.Level != wantLevel {
				t.Fatalf("unexpected request log: %s", logs.String())
			}
			if tc.path == "/swagger/doc.json" {
				var doc struct {
					Swagger string                     `json:"swagger"`
					Paths   map[string]json.RawMessage `json:"paths"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &doc); err != nil {
					t.Fatal(err)
				}
				if doc.Swagger != "2.0" || doc.Paths["/auth/refresh"] == nil {
					t.Fatal("Swagger must serve the registered API document")
				}
			}
		})
	}
}
