package webapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	"whereiseveryone/internal/webapi/me"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
)

type requestLimitStore struct {
	*sessionStore
	operations int
	status     string
}

func (s *requestLimitStore) NewUser(ctx context.Context, user users.User) (users.User, error) {
	s.operations++
	return s.sessionStore.NewUser(ctx, user)
}

func (s *requestLimitStore) GetUserByUsername(ctx context.Context, username string) (users.User, error) {
	s.operations++
	return s.sessionStore.GetUserByUsername(ctx, username)
}

func (s *requestLimitStore) UpdateStatus(_ context.Context, _ id.ID, status string) error {
	s.operations++
	s.status = status
	return nil
}

func newRequestLimitApp(t *testing.T) (*sessionApp, *requestLimitStore) {
	t.Helper()
	a := newSessionApp(t)
	store := &requestLimitStore{sessionStore: a.store}
	j := jwt.NewJWT(a.clock, []byte("session-test-secret"), 15*time.Minute, 720*time.Hour)
	authRouter := auth.NewMux(store, a.clock, j)
	if err := authRouter.SetPasswordHashCost(bcrypt.MinCost); err != nil {
		t.Fatal(err)
	}
	log := logrus.New()
	log.SetOutput(io.Discard)
	a.echo = webapi.NewEcho("", validator.New(), j, store, webapi.EchoRouters{
		Swagger:    func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) },
		AuthRouter: authRouter,
		MeRouter:   me.NewMux(store, a.clock),
	}, log, false)
	return a, store
}

type countedBody struct {
	io.Reader
	read int
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read += n
	return n, err //nolint:wrapcheck // Preserve reader errors, including io.EOF, for the HTTP body reader.
}

func TestHTTPBodyLimitBeforeBinding(t *testing.T) {
	for _, route := range []struct{ method, path, field string }{
		{http.MethodPost, "/auth/signup", "username"},
		{http.MethodPost, "/auth/login", "username"},
		{http.MethodPost, "/auth/refresh", "refresh_token"},
		{http.MethodPut, "/me/status", "status"},
	} {
		for _, contentLength := range []int64{-1, 0, 1, 2 * 1024 * 1024} {
			t.Run(route.path+"/length="+strconv.FormatInt(contentLength, 10), func(t *testing.T) {
				a, store := newRequestLimitApp(t)
				before := store.user.Auth
				body := &countedBody{Reader: strings.NewReader(`{"` + route.field + `":"` + strings.Repeat("x", 2*1024*1024) + `"}`)}
				req := httptest.NewRequestWithContext(t.Context(), route.method, route.path, body)
				req.ContentLength = contentLength
				if contentLength == -1 {
					req.TransferEncoding = []string{"chunked"}
				}
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				req.Header.Set(echo.HeaderAuthorization, "Bearer "+before.Token)
				response := httptest.NewRecorder()
				a.echo.ServeHTTP(response, req)
				if response.Code != http.StatusRequestEntityTooLarge {
					t.Fatalf("status = %d, want 413: %s", response.Code, response.Body.String())
				}
				if body.read > webapi.MaxRequestBodyBytes+1 || (contentLength > webapi.MaxRequestBodyBytes && body.read != 0) {
					t.Fatalf("read %d bytes of an oversized body", body.read)
				}
				if store.reads != 0 || store.operations != 0 || store.user.Auth != before {
					t.Fatal("oversized body reached persistence or changed the session")
				}
			})
		}
	}
}

func TestHTTPBodyLimitIncludesTrailingBytes(t *testing.T) {
	for _, size := range []int{webapi.MaxRequestBodyBytes, webapi.MaxRequestBodyBytes + 1} {
		t.Run(strconv.FormatInt(int64(size), 10), func(t *testing.T) {
			a, store := newRequestLimitApp(t)
			body := `{"status":"hello"}`
			body += strings.Repeat(" ", size-len(body))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/me/status", strings.NewReader(body))
			req.ContentLength = -1
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+store.user.Auth.Token)
			response := httptest.NewRecorder()
			a.echo.ServeHTTP(response, req)
			want := http.StatusNoContent
			if size > webapi.MaxRequestBodyBytes {
				want = http.StatusRequestEntityTooLarge
			}
			if response.Code != want {
				t.Fatalf("status = %d, want %d", response.Code, want)
			}
			if want == http.StatusRequestEntityTooLarge && (store.operations != 0 || store.reads != 0) {
				t.Fatal("oversized trailing data reached persistence")
			}
		})
	}
}

func TestHTTPAuthFieldLimitsBeforePersistence(t *testing.T) {
	for _, path := range []string{"/auth/signup", "/auth/login", "/auth/refresh"} {
		for _, field := range []struct{ name, value string }{
			{"username", strings.Repeat("u", 65)},
			{"username", strings.Repeat("界", 65)},
			{"device_token", strings.Repeat("d", 257)},
			{"device_token", strings.Repeat("界", 257)},
			{"password", strings.Repeat("p", 73)},
			{"password", strings.Repeat("界", 25)}, // 25 runes, 75 bytes.
			{"refresh_token", strings.Repeat("t", jwt.MaxTokenBytes+1)},
			{"refresh_token", strings.Repeat("界", jwt.MaxTokenBytes/3+1)},
		} {
			if path == "/auth/refresh" && (field.name == "username" || field.name == "password") {
				continue
			}
			if path != "/auth/refresh" && field.name == "refresh_token" {
				continue
			}
			t.Run(path+"/"+field.name+"/"+strconv.FormatInt(int64(len(field.value)), 10), func(t *testing.T) {
				a, store := newRequestLimitApp(t)
				before := store.user.Auth
				body := map[string]string{
					"username": "alice", "password": "test-password", "device_token": "device-a", "refresh_token": store.refresh,
				}
				body[field.name] = field.value
				response := a.request(http.MethodPost, path, "", encodeRequest(t, body))
				if response.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
				}
				if store.reads != 0 || store.operations != 0 || store.user.Auth != before {
					t.Fatal("invalid auth fields reached persistence or changed the session")
				}
			})
		}
	}
}

func TestHTTPStatusLengthLimits(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		want         int
	}{
		{"clear", "", http.StatusNoContent},
		{"at limit", strings.Repeat("s", 1024), http.StatusNoContent},
		{"unicode at limit", strings.Repeat("界", 1024), http.StatusNoContent},
		{"over limit", strings.Repeat("s", 1025), http.StatusBadRequest},
		{"unicode over limit", strings.Repeat("界", 1025), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, store := newRequestLimitApp(t)
			response := a.request(http.MethodPut, "/me/status", store.user.Auth.Token, encodeRequest(t, map[string]string{"status": tc.status}))
			if response.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
			if tc.want == http.StatusNoContent {
				if store.operations != 1 || store.status != tc.status {
					t.Fatal("valid status was not persisted")
				}
			} else if store.operations != 0 {
				t.Fatal("oversized status was persisted")
			}
		})
	}
}

func TestHTTPFriendUsernameLengthLimits(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/me/friend"},
		{http.MethodDelete, "/me/friend"},
		{http.MethodPost, "/me/friend/accept"},
		{http.MethodPost, "/me/friend/reject"},
		{http.MethodPost, "/me/sharing/stop"},
		{http.MethodPost, "/me/sharing/resume"},
	} {
		t.Run(route.method+route.path, func(t *testing.T) {
			a, store := newRequestLimitApp(t)
			response := a.request(route.method, route.path, store.user.Auth.Token,
				encodeRequest(t, map[string]string{"username": strings.Repeat("u", 65)}))
			if response.Code != http.StatusBadRequest || store.operations != 0 {
				t.Fatalf("status = %d, persistence operations = %d; want 400 and no operations", response.Code, store.operations)
			}
		})
	}
}

func TestHTTPJSONRoutesRejectOtherMediaTypes(t *testing.T) {
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/auth/signup"},
		{http.MethodPost, "/auth/login"},
		{http.MethodPost, "/auth/refresh"},
		{http.MethodPut, "/me/status"},
		{http.MethodPut, "/me/location"},
		{http.MethodPost, "/me/friend"},
		{http.MethodDelete, "/me/friend"},
		{http.MethodPost, "/me/friend/accept"},
		{http.MethodPost, "/me/friend/reject"},
		{http.MethodPost, "/me/sharing/stop"},
		{http.MethodPost, "/me/sharing/resume"},
	} {
		for _, contentType := range []string{"", echo.MIMEApplicationXML, echo.MIMEApplicationForm, "multipart/form-data; boundary=test", "text/plain", "application/json; charset"} {
			t.Run(route.method+route.path+"/"+contentType, func(t *testing.T) {
				a, store := newRequestLimitApp(t)
				before := store.user.Auth
				req := httptest.NewRequestWithContext(t.Context(), route.method, route.path, strings.NewReader(`{"status":"hello"}`))
				req.Header.Set(echo.HeaderContentType, contentType)
				req.Header.Set(echo.HeaderAuthorization, "Bearer "+before.Token)
				response := httptest.NewRecorder()
				a.echo.ServeHTTP(response, req)
				if response.Code != http.StatusUnsupportedMediaType || store.operations != 0 || store.user.Auth != before {
					t.Fatalf("status = %d, operations = %d; want 415 and no operations", response.Code, store.operations)
				}
			})
		}
	}
}

func TestHTTPAuthFieldBoundaries(t *testing.T) {
	for _, character := range []string{"a", "界"} {
		t.Run(character, func(t *testing.T) {
			a, store := newRequestLimitApp(t)
			body := map[string]string{
				"username": strings.Repeat(character, 64), "password": strings.Repeat("p", 72),
				"device_token": strings.Repeat(character, 256),
			}
			for _, path := range []string{"/auth/signup", "/auth/login", "/auth/refresh"} {
				body["refresh_token"] = store.refresh
				response := a.request(http.MethodPost, path, "", encodeRequest(t, body))
				if response.Code != http.StatusOK {
					t.Fatalf("%s status = %d, want 200: %s", path, response.Code, response.Body.String())
				}
				if len(store.user.Auth.Token) > jwt.MaxTokenBytes || len(store.refresh) > jwt.MaxTokenBytes {
					t.Fatal("maximum-length username produced an oversized JWT")
				}
			}
		})
	}
}

func TestHTTPJSONMediaTypeParameters(t *testing.T) {
	for _, contentType := range []string{"application/json", "application/json; charset=utf-8", "Application/JSON; charset=utf-8"} {
		t.Run(contentType, func(t *testing.T) {
			a, store := newRequestLimitApp(t)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/me/status", strings.NewReader(`{"status":"hello"}`))
			req.Header.Set(echo.HeaderContentType, contentType)
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+store.user.Auth.Token)
			response := httptest.NewRecorder()
			a.echo.ServeHTTP(response, req)
			if response.Code != http.StatusNoContent || store.status != "hello" {
				t.Fatalf("status = %d, stored status = %q", response.Code, store.status)
			}
		})
	}
}

func TestHTTPBearerTokenSizeLimit(t *testing.T) {
	a := newSessionApp(t)
	response := a.request(http.MethodGet, "/me/probe", strings.Repeat("t", jwt.MaxTokenBytes+1), "")
	if response.Code != http.StatusForbidden || a.store.reads != 0 || a.probe.calls != 0 {
		t.Fatalf("status = %d, session reads = %d, handler calls = %d", response.Code, a.store.reads, a.probe.calls)
	}
}

func encodeRequest(t *testing.T, body map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
