package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"whereiseveryone/internal/config"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	"whereiseveryone/pkg/crypto"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
)

type authClock struct{ now time.Time }

func (c *authClock) Now() time.Time { return c.now }

type authStore struct {
	users.Adapter
	accounts     map[string]users.User
	lookupError  error
	beforeLookup func()
	beforeWrite  func()
	lookups      atomic.Int32
	creates      atomic.Int32
	writes       atomic.Int32
}

func (s *authStore) GetUserByUsername(_ context.Context, username string) (users.User, error) {
	s.lookups.Add(1)
	if s.beforeLookup != nil {
		s.beforeLookup()
	}
	if s.lookupError != nil {
		return users.User{}, s.lookupError
	}
	if u, exists := s.accounts[username]; exists {
		return u, nil
	}
	return users.User{}, users.ErrUserNotExists
}

func (s *authStore) NewUser(_ context.Context, user users.User) (users.User, error) {
	s.creates.Add(1)
	if _, exists := s.accounts[user.Auth.Username]; exists {
		return users.User{}, users.ErrUserNameAlreadyExists
	}
	user.ID = id.NewID()
	return user, nil
}

func (s *authStore) UpdateTokens(_ context.Context, _ id.ID, _, _, _ *string) error {
	s.writes.Add(1)
	if s.beforeWrite != nil {
		s.beforeWrite()
	}
	return nil
}

type emptyAuthRouter struct{}

func (emptyAuthRouter) Route(_ *echo.Group, _ echo.MiddlewareFunc) {}

type authApp struct {
	e     *echo.Echo
	m     *mux
	clock *authClock
	store *authStore
}

func newAuthApp(t *testing.T) *authApp {
	t.Helper()
	clock := &authClock{time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)}
	hash, err := crypto.HashPasswordWithCost("test-password", bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	store := &authStore{accounts: map[string]users.User{
		"alice": {ID: id.NewID(), Auth: users.Auth{Username: "alice", Password: hash, DeviceToken: "device-a"}},
	}}
	j := jwt.NewJWT(clock, []byte("test-secret"), time.Minute, time.Hour)
	m := NewMux(store, clock, j)
	if err := m.SetPasswordHashCost(bcrypt.MinCost); err != nil {
		t.Fatal(err)
	}
	log := logrus.New()
	log.SetOutput(io.Discard)
	e := webapi.NewEcho("", validator.New(), j, nil, webapi.EchoRouters{
		Swagger: func(c *echo.Context) error { return c.NoContent(204) }, AuthRouter: m, MeRouter: emptyAuthRouter{},
	}, log, false)
	return &authApp{e, m, clock, store}
}

func (a *authApp) request(ctx context.Context, path, source, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = source
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	// These untrusted headers must not permit a caller to select a new budget.
	req.Header.Set(echo.HeaderXForwardedFor, fmt.Sprintf("198.51.100.%d", len(body)%200+1))
	req.Header.Set(echo.HeaderXRealIP, "198.51.100.250")
	response := httptest.NewRecorder()
	a.e.ServeHTTP(response, req)
	return response
}

func credentials(t *testing.T, username, password string) string {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password, "device_token": "device-a"})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func requireThrottled(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After = %q: %s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
	}
}

func TestLoginFailuresHaveIdenticalResponsesAndBcryptWork(t *testing.T) {
	a := newAuthApp(t)
	var hashes []string
	a.m.verifyPassword = func(hash, password string) error {
		hashes = append(hashes, hash)
		return crypto.VerifyPassword(hash, password)
	}
	known := a.request(t.Context(), "/auth/login", "192.0.2.1:1234", credentials(t, "alice", "wrong"))
	missing := a.request(t.Context(), "/auth/login", "192.0.2.1:1234", credentials(t, "unknown", "wrong"))
	var knownBody, missingBody map[string]any
	if err := json.Unmarshal(known.Body.Bytes(), &knownBody); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(missing.Body.Bytes(), &missingBody); err != nil {
		t.Fatal(err)
	}
	if knownBody["correlation_id"] == "" || missingBody["correlation_id"] == "" ||
		knownBody["correlation_id"] == missingBody["correlation_id"] {
		t.Fatal("login failures must have distinct correlation IDs")
	}
	delete(knownBody, "correlation_id")
	delete(missingBody, "correlation_id")
	if known.Code != http.StatusForbidden || known.Code != missing.Code || !reflect.DeepEqual(knownBody, missingBody) {
		t.Fatalf("login failures differ: known=%d %s, missing=%d %s", known.Code, known.Body.String(), missing.Code, missing.Body.String())
	}
	if len(hashes) != 2 || hashes[0] != a.store.accounts["alice"].Auth.Password || hashes[1] != a.m.dummyPasswordHash {
		t.Fatalf("both failures must run password verification: %v", hashes)
	}
	for _, hash := range hashes {
		if cost, err := bcrypt.Cost([]byte(hash)); err != nil || cost != bcrypt.MinCost {
			t.Fatalf("verification cost = %d, error = %v", cost, err)
		}
	}
	if a.store.writes.Load() != 0 {
		t.Fatal("failed credentials changed a session")
	}
}

func TestMalformedHashesAndMatchingDummyPasswordCannotAuthenticate(t *testing.T) {
	for _, hash := range []string{"", "not-bcrypt", "$2a$04$!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"} {
		t.Run(hash, func(t *testing.T) {
			a := newAuthApp(t)
			u := a.store.accounts["alice"]
			u.Auth.Password = hash
			a.store.accounts["alice"] = u
			dummyCalls := 0
			a.m.verifyPassword = func(encoded, password string) error {
				if encoded == a.m.dummyPasswordHash {
					dummyCalls++
				}
				return crypto.VerifyPassword(encoded, password)
			}
			for _, username := range []string{"alice", "unknown"} {
				response := a.request(t.Context(), "/auth/login", "192.0.2.1:1234",
					credentials(t, username, "dummy-password-never-authenticates"))
				if response.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", response.Code)
				}
			}
			if dummyCalls != 2 || a.store.writes.Load() != 0 {
				t.Fatalf("dummy calls = %d, writes = %d", dummyCalls, a.store.writes.Load())
			}
		})
	}
}

func TestLoginAccountBudgetAcrossSourcesAndAfterExpiry(t *testing.T) {
	for _, username := range []string{"alice", "unknown"} {
		t.Run(username, func(t *testing.T) {
			a := newAuthApp(t)
			body := credentials(t, username, "wrong")
			for i := range loginAccountLimit {
				response := a.request(t.Context(), "/auth/login", fmt.Sprintf("192.0.2.%d:1234", i+1), body)
				if response.Code != http.StatusForbidden {
					t.Fatalf("attempt %d status = %d", i, response.Code)
				}
			}
			requireThrottled(t, a.request(t.Context(), "/auth/login", "198.51.100.1:1234", body))
			if a.store.lookups.Load() != loginAccountLimit || a.store.writes.Load() != 0 {
				t.Fatal("throttled login reached persistence")
			}
			a.clock.now = a.clock.now.Add(15 * time.Minute)
			if response := a.request(t.Context(), "/auth/login", "198.51.100.1:1234", body); response.Code != http.StatusForbidden {
				t.Fatalf("expired account budget status = %d", response.Code)
			}
		})
	}
}

func TestSourceBudgetsCoverInvalidRequestsAndIgnoreForwardedHeaders(t *testing.T) {
	for _, tc := range []struct {
		path   string
		limit  int
		window time.Duration
	}{
		{"/auth/login", loginSourceLimit, 5 * time.Minute},
		{"/auth/signup", signupSourceLimit, time.Hour},
	} {
		t.Run(tc.path, func(t *testing.T) {
			a := newAuthApp(t)
			for i := range tc.limit {
				response := a.request(t.Context(), tc.path, fmt.Sprintf("192.0.2.1:%d", 1000+i), strings.Repeat(" ", i)+"{")
				if response.Code != http.StatusBadRequest {
					t.Fatalf("attempt %d status = %d", i, response.Code)
				}
			}
			requireThrottled(t, a.request(t.Context(), tc.path, "[::ffff:192.0.2.1]:1234", credentials(t, "new", "test-password")))
			if a.store.lookups.Load() != 0 || a.store.creates.Load() != 0 {
				t.Fatal("source-throttled request reached persistence")
			}
			if response := a.request(t.Context(), tc.path, "192.0.2.2:1234", "{"); response.Code != http.StatusBadRequest {
				t.Fatal("independent source shared another source's budget")
			}
			a.clock.now = a.clock.now.Add(tc.window)
			if response := a.request(t.Context(), tc.path, "192.0.2.1:1234", "{"); response.Code != http.StatusBadRequest {
				t.Fatalf("expired source budget status = %d", response.Code)
			}
		})
	}
}

func TestSignupAccountAndGlobalQuotas(t *testing.T) {
	t.Run("account across sources", func(t *testing.T) {
		a := newAuthApp(t)
		body := credentials(t, "alice", "test-password")
		for i := range signupAccountLimit {
			response := a.request(t.Context(), "/auth/signup", fmt.Sprintf("192.0.2.%d:1234", i+1), body)
			if response.Code != http.StatusConflict {
				t.Fatalf("signup status = %d, want 409", response.Code)
			}
		}
		requireThrottled(t, a.request(t.Context(), "/auth/signup", "198.51.100.1:1234", body))
		if a.store.creates.Load() != signupAccountLimit {
			t.Fatalf("signup persistence calls = %d", a.store.creates.Load())
		}
	})
	t.Run("global across sources and usernames", func(t *testing.T) {
		a := newAuthApp(t)
		for i := range signupGlobalLimit {
			if response := a.request(t.Context(), "/auth/signup", fmt.Sprintf("192.0.2.%d:1234", i+1), "{"); response.Code != http.StatusBadRequest {
				t.Fatalf("attempt %d status = %d", i, response.Code)
			}
		}
		requireThrottled(t, a.request(t.Context(), "/auth/signup", "198.51.100.1:1234", credentials(t, "new", "test-password")))
		if a.store.creates.Load() != 0 {
			t.Fatal("global quota did not precede hashing/persistence")
		}
		a.clock.now = a.clock.now.Add(time.Hour)
		if response := a.request(t.Context(), "/auth/signup", "198.51.100.1:1234", "{"); response.Code != http.StatusBadRequest {
			t.Fatal("global signup quota did not expire")
		}
	})
}

func TestAdmissionRejectsImmediatelyBeforeLookupAndHashing(t *testing.T) {
	a := newAuthApp(t)
	a.m.passwordOps = make(chan struct{}, 1)
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	a.store.beforeLookup = func() { close(started); <-release }
	first := make(chan *httptest.ResponseRecorder, 1)
	body := credentials(t, "alice", "wrong")
	go func() { first <- a.request(t.Context(), "/auth/login", "192.0.2.1:1234", body) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first lookup did not start")
	}
	for _, path := range []string{"/auth/login", "/auth/signup"} {
		response := a.request(t.Context(), path, "192.0.2.2:1234", credentials(t, "new", "test-password"))
		requireThrottled(t, response)
	}
	if a.store.lookups.Load() != 1 || a.store.creates.Load() != 0 {
		t.Fatal("excess work entered the database or bcrypt queue")
	}
	unblock()
	select {
	case response := <-first:
		if response.Code != http.StatusForbidden {
			t.Fatalf("first login status = %d", response.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admitted login did not finish")
	}
	a.store.beforeLookup = nil
	if response := a.request(t.Context(), "/auth/login", "192.0.2.2:1234", credentials(t, "new", "wrong")); response.Code != http.StatusForbidden {
		t.Fatal("admission slot was not released after a credential failure")
	}
}

func TestAdmissionHeldThroughPersistenceAndReleasedOnErrors(t *testing.T) {
	a := newAuthApp(t)
	a.m.passwordOps = make(chan struct{}, 1)
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	a.store.beforeWrite = func() { close(started); <-release }
	first := make(chan *httptest.ResponseRecorder, 1)
	body := credentials(t, "alice", "test-password")
	go func() { first <- a.request(t.Context(), "/auth/login", "192.0.2.1:1234", body) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("session write did not start")
	}
	requireThrottled(t, a.request(t.Context(), "/auth/signup", "192.0.2.2:1234", credentials(t, "new", "test-password")))
	unblock()
	if response := <-first; response.Code != http.StatusOK {
		t.Fatalf("valid login status = %d", response.Code)
	}
	a.store.beforeWrite = nil
	a.store.lookupError = errors.New("database failure")
	if response := a.request(t.Context(), "/auth/login", "192.0.2.1:1234", body); response.Code != http.StatusInternalServerError {
		t.Fatalf("database failure status = %d", response.Code)
	}
	a.store.lookupError = nil
	if response := a.request(t.Context(), "/auth/login", "192.0.2.1:1234", body); response.Code != http.StatusOK {
		t.Fatal("admission slot leaked on database failure")
	}
}

func TestDummyHashUsesConfiguredCost(t *testing.T) {
	a := newAuthApp(t)
	limits := config.PasswordWorkLimits{HashCost: bcrypt.MinCost + 1, MaxRequests: 2}
	if err := a.m.SetPasswordWorkLimits(limits); err != nil {
		t.Fatal(err)
	}
	if cap(a.m.passwordOps) != limits.MaxRequests || a.m.passwordHashCost != limits.HashCost {
		t.Fatal("configured password cost and concurrency were not applied together")
	}
	if cost, err := bcrypt.Cost([]byte(a.m.dummyPasswordHash)); err != nil || cost != bcrypt.MinCost+1 {
		t.Fatalf("dummy hash cost = %d, error = %v", cost, err)
	}
	before := a.m.dummyPasswordHash
	for _, cost := range []int{bcrypt.MinCost - 1, config.MaxBcryptCost + 1, bcrypt.MaxCost} {
		if err := a.m.SetPasswordHashCost(cost); err == nil || a.m.dummyPasswordHash != before {
			t.Fatal("invalid cost changed authentication setup")
		}
	}
}

func TestLegacyHashVerificationPadsBcryptWork(t *testing.T) {
	a := newAuthApp(t)
	if err := a.m.SetPasswordHashCost(bcrypt.MinCost + 2); err != nil {
		t.Fatal(err)
	}
	var rounds int
	a.m.verifyPassword = func(hash, password string) error {
		cost, err := bcrypt.Cost([]byte(hash))
		if err != nil {
			t.Fatal(err)
		}
		rounds += 1 << cost
		return crypto.VerifyPassword(hash, password)
	}
	for _, username := range []string{"alice", "unknown"} {
		rounds = 0
		response := a.request(t.Context(), "/auth/login", "192.0.2.1:1234", credentials(t, username, "wrong"))
		if response.Code != http.StatusForbidden || rounds != 1<<(bcrypt.MinCost+2) {
			t.Fatalf("%s status = %d, bcrypt rounds = %d", username, response.Code, rounds)
		}
	}
	if response := a.request(t.Context(), "/auth/login", "192.0.2.1:1234", credentials(t, "alice", "test-password")); response.Code != http.StatusOK {
		t.Fatal("padding prevented valid legacy credentials from authenticating")
	}
}

func TestPasswordAdmissionAndDefaultDummyCost(t *testing.T) {
	m := NewMux(nil, &authClock{}, nil)
	if cost, err := bcrypt.Cost([]byte(m.dummyPasswordHash)); err != nil || cost != crypto.DefaultPasswordHashCost {
		t.Fatalf("default dummy cost = %d, error = %v", cost, err)
	}
	if capacity := cap(m.passwordOps); capacity != 1 {
		t.Fatalf("admission capacity = %d", capacity)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if release, admitted := m.acquirePasswordSlot(ctx); admitted || release != nil || len(m.passwordOps) != 0 {
		t.Fatal("canceled request entered password admission")
	}
}

func TestConfiguredPasswordAdmission(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			a := newAuthApp(t)
			if err := a.m.SetPasswordWorkLimits(config.PasswordWorkLimits{HashCost: bcrypt.MinCost, MaxRequests: limit}); err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}, limit), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			a.store.beforeLookup = func() { started <- struct{}{}; <-release }
			responses := make(chan *httptest.ResponseRecorder, limit)
			body := credentials(t, "alice", "wrong")
			for i := 0; i < limit; i++ {
				go func() { responses <- a.request(t.Context(), "/auth/login", "192.0.2.1:1234", body) }()
			}
			for i := 0; i < limit; i++ {
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("configured number of logins did not enter lookup")
				}
			}
			for _, path := range []string{"/auth/login", "/auth/signup"} {
				requireThrottled(t, a.request(t.Context(), path, "192.0.2.2:1234", credentials(t, "new", "test-password")))
			}
			if int(a.store.lookups.Load()) != limit || a.store.creates.Load() != 0 {
				t.Fatal("excess work entered lookup or signup hashing")
			}
			unblock()
			for i := 0; i < limit; i++ {
				select {
				case response := <-responses:
					if response.Code != http.StatusForbidden {
						t.Fatalf("admitted login status = %d", response.Code)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("admitted login did not finish")
				}
			}
			a.store.beforeLookup = nil
			if response := a.request(t.Context(), "/auth/login", "192.0.2.2:1234", body); response.Code != http.StatusForbidden {
				t.Fatalf("released slot not reused: status = %d", response.Code)
			}
		})
	}
}

func TestInvalidPasswordWorkLimitsPreserveSetup(t *testing.T) {
	m := NewMux(nil, &authClock{}, nil)
	beforeOps, beforeHash := m.passwordOps, m.dummyPasswordHash
	for _, limits := range []config.PasswordWorkLimits{
		{HashCost: bcrypt.MinCost, MaxRequests: 0},
		{HashCost: bcrypt.MinCost, MaxRequests: -1},
		{HashCost: config.MaxBcryptCost + 1, MaxRequests: 2},
	} {
		if err := m.SetPasswordWorkLimits(limits); err == nil {
			t.Fatalf("invalid limits accepted: %+v", limits)
		}
		if m.passwordOps != beforeOps || m.dummyPasswordHash != beforeHash || m.passwordHashCost != crypto.DefaultPasswordHashCost {
			t.Fatal("invalid limits changed authentication setup")
		}
	}
}

func TestCanceledLoginKeepsPasswordSlotUntilVerificationReturns(t *testing.T) {
	a := newAuthApp(t)
	started, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	signalStarted := sync.OnceFunc(func() { close(started) })
	a.m.verifyPassword = func(hash, password string) error {
		signalStarted()
		<-release
		return crypto.VerifyPassword(hash, password)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := make(chan *httptest.ResponseRecorder, 1)
	body := credentials(t, "alice", "wrong")
	go func() { first <- a.request(ctx, "/auth/login", "192.0.2.1:1234", body) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("password verification did not start")
	}
	cancel()
	requireThrottled(t, a.request(t.Context(), "/auth/signup", "192.0.2.2:1234", credentials(t, "new", "test-password")))
	if a.store.creates.Load() != 0 {
		t.Fatal("cancellation released a slot while password verification was still running")
	}
	unblock()
	select {
	case response := <-first:
		if response.Code != http.StatusForbidden {
			t.Fatalf("canceled invalid login status = %d", response.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled login did not finish after verification returned")
	}
	if response := a.request(t.Context(), "/auth/login", "192.0.2.2:1234", body); response.Code != http.StatusForbidden {
		t.Fatalf("slot not reused after canceled verification returned: status = %d", response.Code)
	}
}
