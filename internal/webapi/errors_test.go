package webapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"

	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	"whereiseveryone/internal/webapi/auth"
	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/internal/webapi/me"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
)

type errorStore struct {
	*sessionStore
	failure     error
	failSession bool
}

func (s *errorStore) GetUser(ctx context.Context, userID id.ID) (users.User, error) {
	if s.failSession {
		return users.User{}, s.failure
	}
	return s.sessionStore.GetUser(ctx, userID)
}

func (s *errorStore) GetSession(ctx context.Context, userID id.ID) (users.Auth, error) {
	if s.failSession {
		return users.Auth{}, s.failure
	}
	return s.sessionStore.GetSession(ctx, userID)
}

func (s *errorStore) GetUserByUsername(_ context.Context, _ string) (users.User, error) {
	return users.User{}, s.failure
}

func (s *errorStore) NewUser(_ context.Context, _ users.User) (users.User, error) {
	return users.User{}, s.failure
}

func (s *errorStore) UpdateStatus(_ context.Context, _ id.ID, _ string) error { return s.failure }

type errorRouter struct{ failure error }

func (r errorRouter) Route(g *echo.Group, _ echo.MiddlewareFunc) {
	g.GET("/plain-error", func(_ *echo.Context) error { return r.failure })
	g.GET("/framework-error", func(_ *echo.Context) error {
		return echo.NewHTTPError(503, "private topology details").Wrap(r.failure)
	})
	g.GET("/framework-conflict", func(_ *echo.Context) error {
		return echo.NewHTTPError(409, "private operation details").Wrap(r.failure)
	})
	g.GET("/json-error", func(c *echo.Context) error { return jsonerr.EchoInternalError(r.failure).Echo(c) })
	g.GET("/json-conflict", func(c *echo.Context) error { return jsonerr.EchoConflictError(r.failure).Echo(c) })
	g.GET("/custom-error", func(_ *echo.Context) error { return customPrivateError{r.failure} })
}

type customPrivateError struct{ error }

func (customPrivateError) StatusCode() int { return 500 }
func (customPrivateError) MarshalJSON() ([]byte, error) {
	return []byte(`{"message":"private custom error details"}`), nil
}

func TestPublicErrorsHaveCorrelatedPrivateLogsInBothDebugModes(t *testing.T) {
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprintf("debug=%t", debug), func(t *testing.T) {
			a := newSessionApp(t)
			failure := errors.New("MongoDB operation failed: server selection timeout at db-private:27017 replicaSet=private-rs")
			store := &errorStore{sessionStore: a.store, failure: failure}
			j := jwt.NewJWT(a.clock, []byte("session-test-secret"), 15*time.Minute, 720*time.Hour)
			authRouter := auth.NewMux(store, a.clock, j)
			if err := authRouter.SetPasswordHashCost(bcrypt.MinCost); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			log := logrus.New()
			log.SetOutput(&logs)
			log.SetFormatter(&logrus.JSONFormatter{})
			e := webapi.NewEcho("", validator.New(), j, store, webapi.EchoRouters{
				Swagger:    func(c *echo.Context) error { return c.NoContent(204) },
				AuthRouter: authRouter, MeRouter: me.NewMux(store, a.clock),
			}, log, debug)
			errorRouter{failure}.Route(e.Group("/test"), nil)
			seen := make(map[string]bool)
			for _, tc := range []struct {
				method, path, body, message string
				status                      int
				failSession, underlying     bool
			}{
				{method: "GET", path: "/test/plain-error", status: 500, message: "internal error", underlying: true},
				{method: "GET", path: "/test/framework-error", status: 503, message: "internal error", underlying: true},
				{method: "GET", path: "/test/framework-conflict", status: 409, message: "Conflict", underlying: true},
				{method: "GET", path: "/test/json-error", status: 500, message: "internal error", underlying: true},
				{method: "GET", path: "/test/json-conflict", status: 409, message: "conflict", underlying: true},
				{method: "GET", path: "/test/custom-error", status: 500, message: "internal error", underlying: true},
				{method: "POST", path: "/auth/login", body: `{"username":"alice","password":"test-password","device_token":"device-a"}`, status: 500, message: "internal error", underlying: true},
				{method: "POST", path: "/auth/signup", body: `{"username":"new-user","password":"test-password","device_token":"device-a"}`, status: 500, message: "internal error", underlying: true},
				{method: "POST", path: "/auth/refresh", body: `{"refresh_token":"` + a.store.refresh + `","device_token":"device-a"}`, status: 500, message: "internal error", failSession: true, underlying: true},
				{method: "PUT", path: "/me/status", body: `{"status":"hello"}`, status: 500, message: "internal error", underlying: true},
				{method: "GET", path: "/me/friends", status: 500, message: "internal error", failSession: true, underlying: true},
				{method: "POST", path: "/auth/login", body: strings.Repeat(" ", webapi.MaxRequestBodyBytes+1), status: 413, message: "request body too large"},
				{method: "GET", path: "/missing", status: 404, message: "Not Found"},
				{method: "HEAD", path: "/missing", status: 404},
				{method: "GET", path: "/health", status: 200},
			} {
				t.Run(tc.method+tc.path+fmt.Sprint(tc.status), func(t *testing.T) {
					logs.Reset()
					store.failSession = tc.failSession
					req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, strings.NewReader(tc.body))
					req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
					req.Header.Set(echo.HeaderAuthorization, "Bearer "+a.store.user.Auth.Token)
					req.Header.Set(echo.HeaderXRequestID, "untrusted-client-id")
					response := httptest.NewRecorder()
					e.ServeHTTP(response, req)
					if response.Code != tc.status {
						t.Fatalf("status = %d, want %d: %s", response.Code, tc.status, response.Body.String())
					}
					requestID := response.Header().Get(echo.HeaderXRequestID)
					if requestID == "" || requestID == "untrusted-client-id" || seen[requestID] {
						t.Fatalf("invalid correlation ID: %q", requestID)
					}
					seen[requestID] = true
					if tc.method == http.MethodHead {
						if response.Body.Len() != 0 {
							t.Fatal("HEAD response must not have a body")
						}
					} else if tc.status >= 400 {
						var body map[string]any
						if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
							t.Fatal(err)
						}
						if len(body) != 3 || body["message"] != tc.message || body["code"] != float64(tc.status) || body["correlation_id"] != requestID {
							t.Fatalf("unexpected public error: %s", response.Body.String())
						}
					}
					var entry struct {
						Status        int    `json:"status"`
						CorrelationID string `json:"correlation_id"`
						Error         string `json:"error"`
					}
					if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
						t.Fatalf("expected one structured request log: %v: %s", err, logs.String())
					}
					if entry.Status != tc.status || entry.CorrelationID != requestID ||
						(tc.underlying && !strings.Contains(entry.Error, failure.Error())) {
						t.Fatalf("missing correlated error details: %s", logs.String())
					}
				})
			}
		})
	}
}
