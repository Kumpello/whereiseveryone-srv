package webapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	mu            sync.Mutex
	clock         *sessionClock
	refresh       string
	beforeReplace func()
	user          users.User
	getError      error
	reads         int
}

func (s *sessionStore) GetUser(ctx context.Context, userID id.ID) (users.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if username != s.user.Auth.Username {
		return users.User{}, users.ErrUserNotExists
	}
	return s.user, nil
}

func (s *sessionStore) NewUser(_ context.Context, user users.User) (users.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user.ID = id.NewID()
	s.user = user
	return user, nil
}

func (s *sessionStore) UpdateTokens(_ context.Context, _ id.ID, access, refresh, device *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.user.Auth.PreviousAccessDigest = ""
	s.user.Auth.PreviousAccessValidUntil = time.Time{}
	if access != nil {
		s.user.Auth.Token = *access
	}
	if refresh != nil {
		s.refresh = *refresh
		s.user.Auth.RefreshToken = ""
		s.user.Auth.RefreshTokenDigest = users.TokenDigest(*refresh)
	}
	if device != nil {
		s.user.Auth.DeviceToken = *device
	}
	return nil
}

func (s *sessionStore) ReplaceTokens(_ context.Context, userID id.ID, previous users.Auth, access, refresh, device string) error {
	if s.beforeReplace != nil {
		s.beforeReplace()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if userID != s.user.ID || previous.Token != s.user.Auth.Token ||
		previous.RefreshTokenDigest != s.user.Auth.RefreshTokenDigest || previous.RefreshToken != s.user.Auth.RefreshToken ||
		previous.DeviceToken != s.user.Auth.DeviceToken {
		return users.ErrSessionChanged
	}
	s.user.Auth.Token = access
	s.refresh = refresh
	s.user.Auth.RefreshToken = ""
	s.user.Auth.RefreshTokenDigest = users.TokenDigest(refresh)
	s.user.Auth.DeviceToken = device
	s.user.Auth.PreviousAccessDigest = ""
	s.user.Auth.PreviousAccessValidUntil = time.Time{}
	if device != "" && device == previous.DeviceToken {
		s.user.Auth.PreviousAccessDigest = users.TokenDigest(previous.Token)
		s.user.Auth.PreviousAccessValidUntil = s.clock.Now().Add(users.AccessRotationGrace)
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
	store := &sessionStore{clock: clock, user: users.User{ID: id.NewID(), Auth: users.Auth{Username: "alice", DeviceToken: "device-a"}}}
	password, err := bcrypt.GenerateFromPassword([]byte("test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store.user.Auth.Password = string(password)
	access, refresh, err := j.GenerateTokens("alice", store.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.user.Auth.Token, store.refresh = access, refresh
	store.user.Auth.RefreshTokenDigest = users.TokenDigest(refresh)
	probe := &sessionProbe{}
	log := logrus.New()
	log.SetOutput(io.Discard)
	authRouter := auth.NewMux(store, clock, j)
	authRouter.SetPasswordHashCost(bcrypt.MinCost)
	e := webapi.NewEcho("", validator.New(), j, store, webapi.EchoRouters{
		Swagger:    func(c echo.Context) error { return c.NoContent(204) },
		AuthRouter: authRouter, MeRouter: probe,
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
		{"refresh bearer", func(a *sessionApp) string { return a.store.refresh }, 403, 0},
		{"refresh bearer after one day", func(a *sessionApp) string {
			a.clock.now = a.clock.now.Add(24 * time.Hour)
			return a.store.refresh
		}, 403, 0},
		{"blank device binding", func(a *sessionApp) string {
			a.store.user.Auth.DeviceToken = " \t\n"
			return a.store.user.Auth.Token
		}, 403, 1},
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
			a.store.user.Auth.Token, a.store.refresh = token, token
			a.store.user.Auth.RefreshTokenDigest = users.TokenDigest(token)
			res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+token+`","device_token":"device-a"}`)
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
	for _, action := range []string{"refresh", "login", "device conflict", "login device conflict"} {
		t.Run(action, func(t *testing.T) {
			a := newSessionApp(t)
			oldAccess, oldRefresh := a.store.user.Auth.Token, a.store.refresh
			if res := a.request(http.MethodGet, "/me/probe", oldAccess, ""); res.Code != 204 {
				t.Fatalf("initial access status = %d", res.Code)
			}
			path := "/auth/refresh"
			body := `{"refresh_token":"` + oldRefresh + `","device_token":"device-a"}`
			wantCode := 200
			if action == "login" {
				path = "/auth/login"
				body = `{"username":"alice","password":"test-password","device_token":"device-a"}`
			} else if action == "login device conflict" {
				path = "/auth/login"
				body = `{"username":"alice","password":"test-password","device_token":"device-b"}`
				wantCode = 409
			} else if action == "device conflict" {
				body = `{"refresh_token":"` + oldRefresh + `","device_token":"device-b"}`
				wantCode = 409
			}
			res := a.request(http.MethodPost, path, "", body)
			if res.Code != wantCode {
				t.Fatalf("replacement status = %d, want %d: %s", res.Code, wantCode, res.Body.String())
			}
			if a.store.user.Auth.Token == oldAccess || a.store.refresh == oldRefresh {
				t.Fatal("tokens must change even with an unchanged clock")
			}
			if action == "refresh" {
				if res := a.request(http.MethodGet, "/me/probe", oldAccess, ""); res.Code != 204 {
					t.Fatalf("previous access during grace status = %d", res.Code)
				}
				a.clock.now = a.clock.now.Add(users.AccessRotationGrace)
			}
			for _, revoked := range []string{oldAccess, oldRefresh, a.store.refresh} {
				if res := a.request(http.MethodGet, "/me/probe", revoked, ""); res.Code != 403 {
					t.Fatalf("revoked or refresh bearer status = %d", res.Code)
				}
			}
			if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+oldRefresh+`","device_token":"device-a"}`); res.Code != 403 {
				t.Fatalf("old refresh status = %d", res.Code)
			}
			if wantCode == 200 {
				var pair struct {
					Access  string `json:"token"`
					Refresh string `json:"refresh_token"`
				}
				if err := json.Unmarshal(res.Body.Bytes(), &pair); err != nil {
					t.Fatal(err)
				}
				if pair.Access != a.store.user.Auth.Token || pair.Refresh != a.store.refresh {
					t.Fatal("response must contain the persisted tokens")
				}
				if res := a.request(http.MethodGet, "/me/probe", pair.Access, ""); res.Code != 204 {
					t.Fatalf("new access status = %d", res.Code)
				}
			}
		})
	}
}

func TestAuthRequiresDeviceTokenWithoutChangingSession(t *testing.T) {
	for _, path := range []string{"/auth/signup", "/auth/login", "/auth/refresh"} {
		for _, device := range []struct {
			name  string
			value any
		}{
			{name: "omitted"},
			{name: "null"},
			{name: "empty", value: ""},
			{name: "whitespace", value: " \t\r\n"},
		} {
			t.Run(path+"/"+device.name, func(t *testing.T) {
				a := newSessionApp(t)
				before := a.store.user.Auth
				body := map[string]any{
					"username": "alice", "password": "test-password", "refresh_token": a.store.refresh,
				}
				if device.name != "omitted" {
					body["device_token"] = device.value
				}
				encoded, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				res := a.request(http.MethodPost, path, "", string(encoded))
				if res.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400", res.Code)
				}
				if a.store.user.Auth != before {
					t.Fatal("invalid auth request changed the existing session")
				}
			})
		}
	}
}

func TestUnboundSessionRequiresPasswordLogin(t *testing.T) {
	a := newSessionApp(t)
	a.store.user.Auth.DeviceToken = ""
	oldAccess, oldRefresh := a.store.user.Auth.Token, a.store.refresh
	if res := a.request(http.MethodGet, "/me/probe", oldAccess, ""); res.Code != http.StatusForbidden {
		t.Fatalf("unbound access status = %d, want 403", res.Code)
	}
	if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+oldRefresh+`","device_token":"device-b"}`); res.Code != http.StatusForbidden {
		t.Fatalf("unbound refresh status = %d, want 403", res.Code)
	}
	if a.store.user.Auth.DeviceToken != "" || a.store.refresh != oldRefresh {
		t.Fatal("refresh must not bind an unbound session")
	}
	res := a.request(http.MethodPost, "/auth/login", "", `{"username":"alice","password":"test-password","device_token":"device-b"}`)
	if res.Code != http.StatusOK || a.store.user.Auth.DeviceToken != "device-b" {
		t.Fatalf("password login did not bind the new session: status %d", res.Code)
	}
	if res := a.request(http.MethodGet, "/me/probe", a.store.user.Auth.Token, ""); res.Code != http.StatusNoContent {
		t.Fatalf("bound access status = %d, want 204", res.Code)
	}
	if res := a.request(http.MethodGet, "/me/probe", oldAccess, ""); res.Code != http.StatusForbidden {
		t.Fatalf("old access status = %d, want 403", res.Code)
	}
	if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+oldRefresh+`","device_token":"device-b"}`); res.Code != http.StatusForbidden {
		t.Fatalf("old refresh status = %d, want 403", res.Code)
	}
}

func TestSignupBindsDevice(t *testing.T) {
	a := newSessionApp(t)
	res := a.request(http.MethodPost, "/auth/signup", "", `{"username":"bob","password":"test-password","device_token":"device-b"}`)
	if res.Code != http.StatusOK || a.store.user.Auth.DeviceToken != "device-b" {
		t.Fatalf("signup did not bind device: status %d", res.Code)
	}
	if res := a.request(http.MethodGet, "/me/probe", a.store.user.Auth.Token, ""); res.Code != http.StatusNoContent {
		t.Fatalf("signup access status = %d, want 204", res.Code)
	}
}

func TestRefreshLifetimeAndExpiration(t *testing.T) {
	a := newSessionApp(t)
	refresh := a.store.refresh
	a.clock.now = a.clock.now.Add(15 * time.Minute)
	if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+refresh+`","device_token":"device-a"}`); res.Code != 200 {
		t.Fatalf("refresh after access expiration status = %d", res.Code)
	}
	refresh = a.store.refresh
	a.clock.now = a.clock.now.Add(720 * time.Hour)
	if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+refresh+`","device_token":"device-a"}`); res.Code != 401 {
		t.Fatalf("expired refresh status = %d", res.Code)
	}
}

func TestConcurrentRefreshConsumesCredentialOnce(t *testing.T) {
	for _, age := range []time.Duration{0, time.Minute} {
		t.Run(age.String(), func(t *testing.T) {
			a := newSessionApp(t)
			oldRefresh := a.store.refresh
			a.clock.now = a.clock.now.Add(age)
			// Both requests must finish reading the same session before either writes.
			var ready sync.WaitGroup
			ready.Add(2)
			a.store.beforeReplace = func() { ready.Done(); ready.Wait() }
			responses := make(chan *httptest.ResponseRecorder, 2)
			for range 2 {
				go func() {
					responses <- a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+oldRefresh+`","device_token":"device-a"}`)
				}()
			}
			first, second := <-responses, <-responses
			a.store.beforeReplace = nil
			if first.Code != 200 {
				first, second = second, first
			}
			if first.Code != 200 || second.Code != 403 {
				t.Fatalf("concurrent statuses = %d, %d; want one 200 and one 403", first.Code, second.Code)
			}
			var pair struct {
				Access  string `json:"token"`
				Refresh string `json:"refresh_token"`
			}
			if err := json.Unmarshal(first.Body.Bytes(), &pair); err != nil {
				t.Fatal(err)
			}
			if pair.Refresh == oldRefresh || !a.store.user.Auth.MatchesRefresh(pair.Refresh) || pair.Access != a.store.user.Auth.Token {
				t.Fatal("winner must return the unique, persisted replacement")
			}
			if a.store.user.Auth.RefreshToken != "" || a.store.user.Auth.RefreshTokenDigest != users.TokenDigest(pair.Refresh) {
				t.Fatal("only the refresh digest should be persisted")
			}
			if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+oldRefresh+`","device_token":"device-a"}`); res.Code != 403 {
				t.Fatalf("immediate reuse status = %d", res.Code)
			}
		})
	}
}

func TestStaleRefreshCannotOverwriteLoginOrRevokeIt(t *testing.T) {
	for _, device := range []string{"device-a", "device-b"} {
		t.Run(device, func(t *testing.T) {
			a := newSessionApp(t)
			oldRefresh := a.store.refresh
			a.store.beforeReplace = func() {
				res := a.request(http.MethodPost, "/auth/login", "", `{"username":"alice","password":"test-password","device_token":"device-a"}`)
				if res.Code != 200 {
					t.Fatalf("login status = %d", res.Code)
				}
			}
			res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+oldRefresh+`","device_token":"`+device+`"}`)
			if res.Code != 403 || a.store.user.Auth.DeviceToken != "device-a" {
				t.Fatalf("stale refresh status = %d, device = %q", res.Code, a.store.user.Auth.DeviceToken)
			}
		})
	}
}

func TestPreviousAccessGrace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		status  int
	}{
		{"immediately", 0, 204},
		{"just before deadline", 120*time.Second - time.Nanosecond, 204},
		{"at deadline", 120 * time.Second, 403},
		{"after deadline", 121 * time.Second, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newSessionApp(t)
			oldAccess := a.store.user.Auth.Token
			res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+a.store.refresh+`","device_token":"device-a"}`)
			if res.Code != 200 {
				t.Fatalf("refresh status = %d", res.Code)
			}
			a.clock.now = a.clock.now.Add(tc.elapsed)
			if res := a.request(http.MethodGet, "/me/probe", oldAccess, ""); res.Code != tc.status {
				t.Fatalf("old access status = %d, want %d", res.Code, tc.status)
			}
		})
	}
}

func TestPreviousAccessGraceDoesNotExtendExpiration(t *testing.T) {
	a := newSessionApp(t)
	oldAccess := a.store.user.Auth.Token
	a.clock.now = a.clock.now.Add(15*time.Minute - time.Second)
	if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+a.store.refresh+`","device_token":"device-a"}`); res.Code != 200 {
		t.Fatalf("refresh status = %d", res.Code)
	}
	a.clock.now = a.clock.now.Add(time.Second)
	if res := a.request(http.MethodGet, "/me/probe", oldAccess, ""); res.Code != 401 {
		t.Fatalf("expired access during grace status = %d", res.Code)
	}
}

func TestLoginAndConflictClearExistingGrace(t *testing.T) {
	for _, action := range []string{"login", "conflict"} {
		t.Run(action, func(t *testing.T) {
			a := newSessionApp(t)
			oldAccess := a.store.user.Auth.Token
			if res := a.request(http.MethodPost, "/auth/refresh", "", `{"refresh_token":"`+a.store.refresh+`","device_token":"device-a"}`); res.Code != 200 {
				t.Fatal(res.Body.String())
			}
			path, body, want := "/auth/login", `{"username":"alice","password":"test-password","device_token":"device-a"}`, 200
			if action == "conflict" {
				path, body, want = "/auth/refresh", `{"refresh_token":"`+a.store.refresh+`","device_token":"device-b"}`, 409
			}
			if res := a.request(http.MethodPost, path, "", body); res.Code != want {
				t.Fatalf("replacement status = %d", res.Code)
			}
			if res := a.request(http.MethodGet, "/me/probe", oldAccess, ""); res.Code != 403 {
				t.Fatalf("previous access status = %d", res.Code)
			}
		})
	}
}
