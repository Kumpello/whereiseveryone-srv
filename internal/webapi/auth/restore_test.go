package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"
	"github.com/sirupsen/logrus"
	"whereiseveryone/internal/config"
	"whereiseveryone/internal/restore"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
)

func TestRestoreRoutesRequireEnrollmentSessionAndBoundDevice(t *testing.T) {
	clock := &authClock{time.Now().UTC()}
	userID := id.NewID()
	j := jwt.NewJWT(clock, []byte("restore-test-secret"), time.Minute, time.Hour)
	access, refresh, err := j.GenerateTokens("alice", userID)
	if err != nil {
		t.Fatal(err)
	}
	userStore := &restoreRouteUserStore{user: users.User{ID: userID, Auth: users.Auth{
		Username: "alice", Token: access, RefreshTokenDigest: users.TokenDigest(refresh), DeviceToken: "source",
	}}}
	keyStore := &restoreRouteKeyStore{}
	service, err := restore.NewService(config.RestoreCredentials{
		RPID: "example.com", AndroidOrigins: map[string][]string{
			"app.id": {"android:apk-key-hash:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		},
	}, keyStore, clock)
	if err != nil {
		t.Fatal(err)
	}
	m := NewMux(userStore, clock, j)
	m.SetRestoreService(service)
	log := logrus.New()
	log.SetOutput(io.Discard)
	e := webapi.NewEcho("", validator.New(), j, userStore, webapi.EchoRouters{
		Swagger:    func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) },
		AuthRouter: m, MeRouter: emptyAuthRouter{},
	}, log, false)
	request := func(path, token, body, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res := httptest.NewRecorder()
		e.ServeHTTP(res, req)
		return res
	}
	body := `{"app_id":"app.id","device_token":"source"}`
	for _, path := range []string{"/auth/restore/register/options", "/auth/restore/register/finish"} {
		if response := request(path, "", body, "application/json"); response.Code != http.StatusForbidden {
			t.Fatalf("unauthenticated enrollment returned %d", response.Code)
		}
	}
	if response := request("/auth/restore/register/options", access,
		`{"app_id":"app.id","device_token":"other-device"}`, "application/json"); response.Code != http.StatusForbidden {
		t.Fatalf("enrollment from a different device returned %d", response.Code)
	}
	response := request("/auth/restore/register/options", access, body, "application/json")
	if response.Code != http.StatusOK {
		t.Fatalf("valid enrollment returned %d: %s", response.Code, response.Body)
	}
	var result restore.Options
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var creation struct {
		Challenge string `json:"challenge"`
		User      struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err = json.Unmarshal([]byte(result.RequestJSON), &creation); err != nil {
		t.Fatal(err)
	}
	handle, err := base64.RawURLEncoding.DecodeString(creation.User.ID)
	if err != nil || string(handle) != userID.Hex() || creation.Challenge == "" {
		t.Fatal("enrollment options do not identify the authenticated account")
	}
	if keyStore.challenge.UserID != userID || keyStore.challenge.AppID != "app.id" {
		t.Fatal("challenge binding was not persisted")
	}
	if response := request("/auth/restore/login/options", "", body, "application/json"); response.Code != http.StatusOK {
		t.Fatalf("discoverable sign-in requires an old session: %d", response.Code)
	}
	for _, example := range []struct {
		body, media string
		status      int
	}{
		{"{", "application/json", 400},
		{`{"app_id":"app.id","device_token":" "}`, "application/json", 400},
		{body, "text/plain", 415},
		{`{"app_id":"unknown","device_token":"source"}`, "application/json", 503},
		{strings.Repeat(" ", 17*1024), "application/json", 413},
	} {
		if response := request("/auth/restore/login/options", "", example.body, example.media); response.Code != example.status {
			t.Fatalf("invalid restore request returned %d, want %d", response.Code, example.status)
		}
	}
}

func (s *restoreRouteUserStore) GetUser(_ context.Context, userID id.ID) (users.User, error) {
	if s.user.ID != userID {
		return users.User{}, users.ErrUserNotExists
	}
	return s.user, nil
}

func (s *restoreRouteUserStore) GetSession(_ context.Context, userID id.ID) (users.Auth, error) {
	if s.user.ID != userID {
		return users.Auth{}, users.ErrUserNotExists
	}
	return s.user.Auth, nil
}

func (s *restoreRouteKeyStore) SaveChallenge(_ context.Context, challenge restore.Challenge) error {
	s.challenge = challenge
	return nil
}

type restoreRouteUserStore struct {
	users.Adapter
	user users.User
}

type restoreRouteKeyStore struct {
	restore.Store
	challenge restore.Challenge
}
