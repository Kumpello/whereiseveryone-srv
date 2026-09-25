package webapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/validator"
	jwtgo "github.com/golang-jwt/jwt"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	"whereiseveryone/internal/webapi/auth"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
)

type sessionClock struct{ now time.Time }

func (c *sessionClock) Now() time.Time { return c.now }

type sessionStore struct {
	users.Adapter
	user     users.User
	getError error
	reads    int
}

func (s *sessionStore) GetUser(ctx context.Context, userID id.ID) (users.User, error) {
	s.reads++
	if _, ok := ctx.Deadline(); !ok {
		return users.User{}, errors.New("session lookup requires a deadline")
	}
	if s.getError != nil {
		return users.User{}, s.getError
	}
	if userID != s.user.ID {
		return users.User{}, users.ErrUserNotExists
	}
	return s.user, nil
}

func (s *sessionStore) GetUserByUsername(_ context.Context, username string) (users.User, error) {
	if username != s.user.Auth.Username {
		return users.User{}, users.ErrUserNotExists
	}
	return s.user, nil
}

func (s *sessionStore) UpdateTokens(_ context.Context, _ id.ID, access, refresh, device *string) error {
	if access != nil {
		s.user.Auth.Token = *access
	}
	if refresh != nil {
		s.user.Auth.RefreshToken = *refresh
	}
	if device != nil {
		s.user.Auth.DeviceToken = *device
	}
	return nil
}

type sessionProbe struct{ calls int }

func (p *sessionProbe) Route(g *echo.Group, _ echo.MiddlewareFunc) {
	g.GET("/probe", func(c echo.Context) error {
		p.calls++
		if _, err := webapi.GetJWTToken(c); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	})
}

type sessionApp struct {
	echo  *echo.Echo
	clock *sessionClock
	store *sessionStore
	probe *sessionProbe
}

func newSessionApp(t *testing.T) *sessionApp {
	t.Helper()
	clock := &sessionClock{time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)}
	j := jwt.NewJWT(clock, []byte("session-test-secret"), 15*time.Minute, 720*time.Hour)
	store := &sessionStore{user: users.User{ID: id.NewID(), Auth: users.Auth{Username: "alice", DeviceToken: "device-a"}}}
	password, err := bcrypt.GenerateFromPassword([]byte("test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store.user.Auth.Password = string(password)
	access, refresh, err := j.GenerateTokens("alice", store.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.user.Auth.Token, store.user.Auth.RefreshToken = access, refresh
	probe := &sessionProbe{}
	log := logrus.New()
	log.SetOutput(io.Discard)
	e := webapi.NewEcho("", validator.New(), j, store, webapi.EchoRouters{
		Swagger:    func(c echo.Context) error { return c.NoContent(204) },
		AuthRouter: auth.NewMux(store, clock, j), MeRouter: probe,
	}, log, false)
	return &sessionApp{e, clock, store, probe}
}

func (a *sessionApp) request(method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if token != "" {
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	}
	response := httptest.NewRecorder()
	a.echo.ServeHTTP(response, req)
	return response
}

func TestProtectedRoutesRequireCurrentAccessToken(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prepare   func(*sessionApp) string
		wantCode  int
		wantReads int
	}{
		{"current access", func(a *sessionApp) string { return a.store.user.Auth.Token }, 204, 1},
		{"refresh bearer", func(a *sessionApp) string { return a.store.user.Auth.RefreshToken }, 403, 0},
		{"missing bearer", func(a *sessionApp) string { return "" }, 403, 0},
		{"malformed bearer", func(a *sessionApp) string { return "invalid" }, 403, 0},
		{"expired access", func(a *sessionApp) string {
			a.clock.now = a.clock.now.Add(15 * time.Minute)
			return a.store.user.Auth.Token
		}, 401, 0},
		{"cleared session", func(a *sessionApp) string {
			token := a.store.user.Auth.Token
			a.store.user.Auth.Token = ""
			return token
		}, 403, 1},
		{"deleted account", func(a *sessionApp) string {
			a.store.getError = users.ErrUserNotExists
			return a.store.user.Auth.Token
		}, 403, 1},
		{"database failure", func(a *sessionApp) string {
			a.store.getError = errors.New("private database details")
			return a.store.user.Auth.Token
		}, 500, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newSessionApp(t)
			res := a.request(http.MethodGet, "/me/probe", tc.prepare(a), "")
			if res.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d: %s", res.Code, tc.wantCode, res.Body.String())
			}
			wantCalls := 0
			if tc.wantCode == 204 {
				wantCalls = 1
			}
			if a.probe.calls != wantCalls || a.store.reads != tc.wantReads {
				t.Fatalf("handler calls = %d, reads = %d", a.probe.calls, a.store.reads)
			}
			if strings.Contains(res.Body.String(), "private database") {
				t.Fatal("session-store error leaked to response")
			}
		})
	}
}

func TestRefreshRejectsAccessAndLegacyTokens(t *testing.T) {
	for _, kind := range []string{"access", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			a := newSessionApp(t)
			token := a.store.user.Auth.Token
			if kind == "legacy" {
				var err error
				token, err = jwtgo.NewWithClaims(jwtgo.SigningMethodHS256, jwtgo.MapClaims{
					"UserName": "alice", "ID": a.store.user.ID.Hex(), "exp": a.clock.now.Add(720 * time.Hour).Unix(),
				}).SignedString([]byte("session-test-secret"))
				if err != nil {
					t.Fatal(err)
				}
			}
			// Matching stored credentials must not bypass token-purpose validation.
			a.store.user.Auth.Token, a.store.user.Auth.RefreshToken = token, token
			res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+token+`"}`)
			if res.Code != 403 || a.store.reads != 0 {
				t.Fatalf("refresh status = %d, reads = %d", res.Code, a.store.reads)
			}
			if kind == "legacy" {
				res = a.request(http.MethodGet, "/me/probe", token, "")
				if res.Code != 403 || a.probe.calls != 0 || a.store.reads != 0 {
					t.Fatalf("legacy bearer status = %d", res.Code)
				}
			}
		})
	}
}

func TestTokenReplacementRevokesPreviousSession(t *testing.T) {
	for _, action := range []string{"refresh", "login", "device conflict"} {
		t.Run(action, func(t *testing.T) {
			a := newSessionApp(t)
			oldAccess, oldRefresh := a.store.user.Auth.Token, a.store.user.Auth.RefreshToken
			if res := a.request(http.MethodGet, "/me/probe", oldAccess, ""); res.Code != 204 {
				t.Fatalf("initial access status = %d", res.Code)
			}
			path := "/auth/refresh"
			body := `{"refresh_token":"` + oldRefresh + `","device_token":"device-a"}`
			wantCode := 200
			if action == "login" {
				path = "/auth/login"
				body = `{"username":"alice","password":"test-password","device_token":"device-a"}`
			} else if action == "device conflict" {
				body = `{"refresh_token":"` + oldRefresh + `","device_token":"device-b"}`
				wantCode = 409
			}
			res := a.request(http.MethodPost, path, "", body)
			if res.Code != wantCode {
				t.Fatalf("replacement status = %d, want %d: %s", res.Code, wantCode, res.Body.String())
			}
			if a.store.user.Auth.Token == oldAccess || a.store.user.Auth.RefreshToken == oldRefresh {
				t.Fatal("tokens must change even with an unchanged clock")
			}
			for _, revoked := range []string{oldAccess, oldRefresh, a.store.user.Auth.RefreshToken} {
				if res := a.request(http.MethodGet, "/me/probe", revoked, ""); res.Code != 403 {
					t.Fatalf("revoked or refresh bearer status = %d", res.Code)
				}
			}
			if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+oldRefresh+`"}`); res.Code != 403 {
				t.Fatalf("old refresh status = %d", res.Code)
			}
			if action != "device conflict" {
				var pair struct {
					Access  string `json:"token"`
					Refresh string `json:"refresh_token"`
				}
				if err := json.Unmarshal(res.Body.Bytes(), &pair); err != nil {
					t.Fatal(err)
				}
				if pair.Access != a.store.user.Auth.Token || pair.Refresh != a.store.user.Auth.RefreshToken {
					t.Fatal("response must contain the persisted tokens")
				}
				if res := a.request(http.MethodGet, "/me/probe", pair.Access, ""); res.Code != 204 {
					t.Fatalf("new access status = %d", res.Code)
				}
			}
		})
	}
}

func TestRefreshLifetimeAndExpiration(t *testing.T) {
	a := newSessionApp(t)
	refresh := a.store.user.Auth.RefreshToken
	a.clock.now = a.clock.now.Add(15 * time.Minute)
	if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+refresh+`"}`); res.Code != 200 {
		t.Fatalf("refresh after access expiration status = %d", res.Code)
	}
	refresh = a.store.user.Auth.RefreshToken
	a.clock.now = a.clock.now.Add(720 * time.Hour)
	if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+refresh+`"}`); res.Code != 401 {
		t.Fatalf("expired refresh status = %d", res.Code)
	}
}
