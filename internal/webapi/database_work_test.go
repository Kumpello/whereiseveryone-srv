package webapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	"whereiseveryone/internal/webapi/auth"
	"whereiseveryone/internal/webapi/me"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
)

// The observer can stall individual database calls without using MongoDB or sleeps.
type databaseWorkStore struct {
	users.Adapter
	user    users.User
	observe func(context.Context, string) error
}

func (s *databaseWorkStore) GetSession(ctx context.Context, _ id.ID) (users.Auth, error) {
	return s.user.Auth, s.observe(ctx, "session")
}

func (s *databaseWorkStore) GetFriendPage(ctx context.Context, _ id.ID, _ users.FriendPageQuery) (users.FriendPage, error) {
	return users.FriendPage{}, s.observe(ctx, "friends")
}

func (s *databaseWorkStore) UpdateLocation(ctx context.Context, _ id.ID, _ users.Location) error {
	return s.observe(ctx, "location")
}

func (s *databaseWorkStore) GetUser(ctx context.Context, _ id.ID) (users.User, error) {
	return s.user, s.observe(ctx, "refresh read")
}

func (s *databaseWorkStore) GetUserByUsername(ctx context.Context, _ string) (users.User, error) {
	return s.user, s.observe(ctx, "login read")
}

func (s *databaseWorkStore) NewUser(ctx context.Context, user users.User) (users.User, error) {
	user.ID = id.NewID()
	return user, s.observe(ctx, "signup write")
}

func (s *databaseWorkStore) UpdateTokens(ctx context.Context, _ id.ID, _, _, _ *string) error {
	return s.observe(ctx, "auth write")
}

func (s *databaseWorkStore) ReplaceTokens(ctx context.Context, _ id.ID, _ users.Auth, _, _, _ string) error {
	return s.observe(ctx, "refresh write")
}

type databaseWorkApp struct {
	echo     *echo.Echo
	user     users.User
	refresh  string
	clock    *sessionClock
	basePath string
}

func newDatabaseWorkApp(t *testing.T, basePath string, observe func(context.Context, string) error,
	limits ...config.DatabaseRequestLimits,
) *databaseWorkApp {
	t.Helper()
	clock := &sessionClock{time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	j := jwt.NewJWT(clock, []byte("database-admission-test-secret"), 15*time.Minute, time.Hour)
	user := users.User{ID: id.NewID(), Auth: users.Auth{Username: "alice", DeviceToken: "device-a"}}
	access, refresh, err := j.GenerateTokens(user.Auth.Username, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	password, err := bcrypt.GenerateFromPassword([]byte("test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	user.Auth.Token, user.Auth.Password = access, string(password)
	user.Auth.RefreshTokenDigest = users.TokenDigest(refresh)
	store := &databaseWorkStore{user: user, observe: observe}
	log := logrus.New()
	log.SetOutput(io.Discard)
	authRouter := auth.NewMux(store, clock, j)
	if err := authRouter.SetPasswordHashCost(bcrypt.MinCost); err != nil {
		t.Fatal(err)
	}
	e := webapi.NewEcho(basePath, validator.New(), j, store, webapi.EchoRouters{
		Swagger:    func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) },
		AuthRouter: authRouter, MeRouter: me.NewMux(store, clock),
	}, log, false, limits...)
	return &databaseWorkApp{echo: e, user: user, refresh: refresh, clock: clock, basePath: basePath}
}

func (a *databaseWorkApp) request(ctx context.Context, method, path string) *httptest.ResponseRecorder {
	body := ""
	switch path {
	case "/auth/login", "/auth/signup":
		body = `{"username":"alice","password":"test-password","device_token":"device-a"}`
	case "/auth/refresh":
		body = `{"refresh_token":"` + a.refresh + `","device_token":"device-a"}`
	case "/me/location":
		encoded, _ := json.Marshal(map[string]any{"latitude": 52.23, "longitude": 21.01, "last_update": a.clock.now.UnixMilli()})
		body = string(encoded)
	}
	return performanceRequest(ctx, a.echo, method, a.basePath+path, a.user.Auth.Token, body)
}

func awaitDatabaseWork[T any](t *testing.T, result <-chan T) T {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("database request did not respond promptly")
		var zero T
		return zero
	}
}

func requireDatabaseBusy(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	var body struct {
		Code          int    `json:"code"`
		Message       string `json:"message"`
		CorrelationID string `json:"correlation_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" ||
		body.Code != http.StatusServiceUnavailable || body.Message != "internal error" ||
		body.CorrelationID == "" || response.Header().Get(echo.HeaderXRequestID) != body.CorrelationID {
		t.Fatalf("unexpected overload response: %d, %v, %s", response.Code, response.Header(), response.Body.String())
	}
}

func TestDatabaseAdmissionBoundsConcurrentWorkBeforeSessionLookup(t *testing.T) {
	const admitted = 4
	gate := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(gate) })
	defer unblock()
	started := make(chan struct{}, admitted)
	var calls, active, peak atomic.Int64
	a := newDatabaseWorkApp(t, "", func(ctx context.Context, operation string) error {
		calls.Add(1)
		if operation != "session" {
			return nil
		}
		current := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); current > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, current) {
				break
			}
		}
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	responses := make(chan *httptest.ResponseRecorder, admitted)
	for range admitted {
		go func() { responses <- a.request(t.Context(), http.MethodGet, "/me/friends") }()
		awaitDatabaseWork(t, started)
	}
	const excess = 40
	rejected := make(chan *httptest.ResponseRecorder, excess)
	paths := []string{"/me/friends", "/me/location", "/auth/refresh", "/auth/login", "/auth/signup"}
	for i := range excess {
		path := paths[i%len(paths)]
		method := http.MethodPost
		switch path {
		case "/me/friends":
			method = http.MethodGet
		case "/me/location":
			method = http.MethodPut
		}
		go func() { rejected <- a.request(t.Context(), method, path) }()
	}
	for range excess {
		requireDatabaseBusy(t, awaitDatabaseWork(t, rejected))
	}
	if calls.Load() != admitted || peak.Load() != admitted || active.Load() != admitted {
		t.Fatalf("database calls = %d, peak = %d, active = %d", calls.Load(), peak.Load(), active.Load())
	}
	for _, path := range []string{"/health", "/swagger/index.html", "/unrelated"} {
		response := performanceRequest(t.Context(), a.echo, http.MethodGet, path, "", "")
		want := http.StatusOK
		switch path {
		case "/swagger/index.html":
			want = http.StatusNoContent
		case "/unrelated":
			want = http.StatusNotFound
		}
		if response.Code != want {
			t.Fatalf("unrelated route %s returned %d, want %d", path, response.Code, want)
		}
	}
	unblock()
	for range admitted {
		if response := awaitDatabaseWork(t, responses); response.Code != http.StatusOK {
			t.Fatalf("admitted request failed: %d, %s", response.Code, response.Body.String())
		}
	}
	if response := a.request(t.Context(), http.MethodGet, "/me/friends"); response.Code != http.StatusOK {
		t.Fatal("finished requests did not release admission slots")
	}
}

func TestDatabaseAdmissionSharedAcrossAuthAndHandlerWork(t *testing.T) {
	for _, tc := range []struct {
		operation string
		method    string
		path      string
		wantCode  int
	}{
		{"friends", http.MethodGet, "/me/friends", http.StatusOK},
		{"location", http.MethodPut, "/me/location", http.StatusNoContent},
		{"refresh read", http.MethodPost, "/auth/refresh", http.StatusOK},
		{"refresh write", http.MethodPost, "/auth/refresh", http.StatusOK},
		{"login read", http.MethodPost, "/auth/login", http.StatusOK},
		{"auth write", http.MethodPost, "/auth/login", http.StatusOK},
		{"signup write", http.MethodPost, "/auth/signup", http.StatusOK},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			gate, started := make(chan struct{}), make(chan struct{}, 1)
			unblock := sync.OnceFunc(func() { close(gate) })
			defer unblock()
			a := newDatabaseWorkApp(t, "/v1", func(ctx context.Context, operation string) error {
				if operation != tc.operation {
					return nil
				}
				started <- struct{}{}
				select {
				case <-gate:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}, config.DatabaseRequestLimits{MaxRequests: 1, Timeout: time.Second})
			first := make(chan *httptest.ResponseRecorder, 1)
			go func() { first <- a.request(t.Context(), tc.method, tc.path) }()
			awaitDatabaseWork(t, started)
			requireDatabaseBusy(t, a.request(t.Context(), http.MethodGet, "/me/friends"))
			requireDatabaseBusy(t, a.request(t.Context(), http.MethodPost, "/auth/refresh"))
			unblock()
			if response := awaitDatabaseWork(t, first); response.Code != tc.wantCode {
				t.Fatalf("admitted request status = %d, want %d: %s", response.Code, tc.wantCode, response.Body.String())
			}
		})
	}
}

func TestDatabaseAdmissionReleasesAfterErrors(t *testing.T) {
	for _, tc := range []struct {
		operation string
		method    string
		path      string
	}{
		{"session", http.MethodGet, "/me/friends"},
		{"friends", http.MethodGet, "/me/friends"},
		{"location", http.MethodPut, "/me/location"},
		{"refresh read", http.MethodPost, "/auth/refresh"},
		{"refresh write", http.MethodPost, "/auth/refresh"},
		{"login read", http.MethodPost, "/auth/login"},
		{"auth write", http.MethodPost, "/auth/login"},
		{"signup write", http.MethodPost, "/auth/signup"},
	} {
		t.Run(tc.operation, func(t *testing.T) {
			fail := true
			a := newDatabaseWorkApp(t, "", func(_ context.Context, operation string) error {
				if fail && operation == tc.operation {
					return errors.New("private database failure")
				}
				return nil
			}, config.DatabaseRequestLimits{MaxRequests: 1, Timeout: time.Second})
			if response := a.request(t.Context(), tc.method, tc.path); response.Code != http.StatusInternalServerError {
				t.Fatalf("database error status = %d", response.Code)
			}
			fail = false
			if response := a.request(t.Context(), http.MethodGet, "/me/friends"); response.Code != http.StatusOK {
				t.Fatal("database error leaked an admission slot")
			}
		})
	}
}

func TestDatabaseCancellationReleasesSlots(t *testing.T) {
	for _, operation := range []string{"session", "friends", "refresh write"} {
		t.Run(operation, func(t *testing.T) {
			gate, started := make(chan struct{}), make(chan struct{}, 1)
			defer close(gate)
			a := newDatabaseWorkApp(t, "", func(ctx context.Context, current string) error {
				if current != operation {
					return nil
				}
				started <- struct{}{}
				select {
				case <-gate:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}, config.DatabaseRequestLimits{MaxRequests: 1, Timeout: time.Minute})
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			first := make(chan *httptest.ResponseRecorder, 1)
			method, path := http.MethodGet, "/me/friends"
			if operation == "refresh write" {
				method, path = http.MethodPost, "/auth/refresh"
			}
			go func() { first <- a.request(parent, method, path) }()
			awaitDatabaseWork(t, started)
			cancel()
			if response := awaitDatabaseWork(t, first); response.Code != http.StatusInternalServerError {
				t.Fatalf("canceled database request status = %d", response.Code)
			}
			// A fresh request must enter its database call while the adapter is still blocked.
			secondParent, cancelSecond := context.WithCancel(t.Context())
			defer cancelSecond()
			second := make(chan *httptest.ResponseRecorder, 1)
			go func() { second <- a.request(secondParent, method, path) }()
			awaitDatabaseWork(t, started)
			cancelSecond()
			if response := awaitDatabaseWork(t, second); response.Code != http.StatusInternalServerError {
				t.Fatalf("second canceled request status = %d", response.Code)
			}
		})
	}
}

func TestDatabaseRequestDeadlineSpansSessionAndHandler(t *testing.T) {
	for _, tc := range []struct {
		method   string
		path     string
		wantCode int
	}{
		{http.MethodGet, "/me/friends", http.StatusOK},
		{http.MethodPut, "/me/location", http.StatusNoContent},
		{http.MethodPost, "/auth/login", http.StatusOK},
		{http.MethodPost, "/auth/signup", http.StatusOK},
		{http.MethodPost, "/auth/refresh", http.StatusOK},
	} {
		t.Run(tc.path, func(t *testing.T) {
			var contexts []context.Context
			a := newDatabaseWorkApp(t, "", func(ctx context.Context, _ string) error {
				contexts = append(contexts, ctx)
				return nil
			}, config.DatabaseRequestLimits{MaxRequests: 1, Timeout: time.Minute})
			if response := a.request(t.Context(), tc.method, tc.path); response.Code != tc.wantCode {
				t.Fatalf("request status = %d", response.Code)
			}
			if len(contexts) < 2 {
				t.Fatal("expected multiple stages of database work")
			}
			deadline, ok := contexts[0].Deadline()
			if !ok || time.Until(deadline) < 30*time.Second {
				t.Fatalf("configured one-minute request deadline = %v", deadline)
			}
			for _, ctx := range contexts {
				got, hasDeadline := ctx.Deadline()
				if !hasDeadline || got != deadline || ctx.Err() != context.Canceled {
					t.Fatalf("stage deadline = %v, error = %v; want shared deadline %v and cancellation", got, ctx.Err(), deadline)
				}
			}
		})
	}
}

func TestDatabaseAdmissionRejectsCanceledRequestsBeforeWork(t *testing.T) {
	var calls int
	a := newDatabaseWorkApp(t, "", func(_ context.Context, _ string) error {
		calls++
		return nil
	}, config.DatabaseRequestLimits{MaxRequests: 1, Timeout: time.Second})
	parent, cancel := context.WithCancel(t.Context())
	cancel()
	if response := a.request(parent, http.MethodGet, "/me/friends"); response.Code != http.StatusInternalServerError {
		t.Fatalf("canceled request status = %d", response.Code)
	}
	if calls != 0 {
		t.Fatal("an already canceled request started database work")
	}
	if response := a.request(t.Context(), http.MethodGet, "/me/friends"); response.Code != http.StatusOK {
		t.Fatal("an already canceled request consumed the admission slot")
	}
}

func TestDatabaseDeadlinePreservesEarlierParentAndReleasesSlot(t *testing.T) {
	var deadlines []time.Time
	block := true
	a := newDatabaseWorkApp(t, "", func(ctx context.Context, operation string) error {
		got, ok := ctx.Deadline()
		if !ok {
			return errors.New("missing deadline")
		}
		deadlines = append(deadlines, got)
		if block && operation == "friends" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}, config.DatabaseRequestLimits{MaxRequests: 1, Timeout: time.Minute})
	deadline := time.Now().Add(100 * time.Millisecond)
	parent, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	if response := a.request(parent, http.MethodGet, "/me/friends"); response.Code != http.StatusInternalServerError {
		t.Fatalf("timed-out request status = %d", response.Code)
	}
	if len(deadlines) != 2 || deadlines[0] != deadline || deadlines[1] != deadline {
		t.Fatalf("session/handler deadlines = %v, want earlier parent %v", deadlines, deadline)
	}
	block = false
	if response := a.request(t.Context(), http.MethodGet, "/me/friends"); response.Code != http.StatusOK {
		t.Fatal("request deadline leaked an admission slot")
	}
}

func TestDatabaseDeadlineHoldsSlotUntilWorkActuallyStops(t *testing.T) {
	gate, started := make(chan struct{}), make(chan context.Context, 1)
	unblock := sync.OnceFunc(func() { close(gate) })
	defer unblock()
	a := newDatabaseWorkApp(t, "", func(ctx context.Context, operation string) error {
		if operation != "session" {
			return nil
		}
		select {
		case started <- ctx:
		default:
		}
		<-gate // Simulate work that cannot stop immediately on cancellation.
		return ctx.Err()
	}, config.DatabaseRequestLimits{MaxRequests: 1, Timeout: 30 * time.Millisecond})
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- a.request(t.Context(), http.MethodGet, "/me/friends") }()
	ctx := awaitDatabaseWork(t, started)
	awaitDatabaseWork(t, ctx.Done())
	requireDatabaseBusy(t, a.request(t.Context(), http.MethodPost, "/auth/refresh"))
	unblock()
	if response := awaitDatabaseWork(t, first); response.Code != http.StatusInternalServerError {
		t.Fatalf("timed-out request status = %d", response.Code)
	}
	if response := a.request(t.Context(), http.MethodGet, "/me/friends"); response.Code != http.StatusOK {
		t.Fatal("finished work did not release its admission slot")
	}
}
