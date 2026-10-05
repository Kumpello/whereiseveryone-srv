package webapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"
	"github.com/sirupsen/logrus"

	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	"whereiseveryone/internal/webapi/me"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
)

// The immutable adapter isolates HTTP, JWT, logging, and serialization costs.
// Database decoding and network latency are measured by the Mongo benchmarks.
type performanceStore struct {
	users.Adapter
	user         users.User
	members      map[id.ID]users.User
	incoming     []id.ID
	outgoing     []id.ID
	userReads    atomic.Int64
	sessionReads atomic.Int64
	batchReads   atomic.Int64
	listReads    atomic.Int64
}

func (s *performanceStore) GetUser(_ context.Context, _ id.ID) (users.User, error) {
	s.userReads.Add(1)
	return s.user, nil
}

func (s *performanceStore) GetSession(_ context.Context, _ id.ID) (users.Auth, error) {
	s.sessionReads.Add(1)
	return s.user.Auth, nil
}

func (s *performanceStore) GetUsers(_ context.Context, ids []id.ID) ([]users.User, error) {
	s.batchReads.Add(1)
	result := make([]users.User, 0, len(ids))
	for _, userID := range ids {
		result = append(result, s.members[userID])
	}
	return result, nil
}

func (s *performanceStore) GetPendingIncomingFriendRequestUserIDs(_ context.Context, _ id.ID) ([]id.ID, error) {
	s.listReads.Add(1)
	return s.incoming, nil
}

func (s *performanceStore) GetPendingOutgoingFriendRequestUserIDs(_ context.Context, _ id.ID) ([]id.ID, error) {
	s.listReads.Add(1)
	return s.outgoing, nil
}

func (*performanceStore) UpdateLocation(_ context.Context, _ id.ID, _ users.Location) error {
	return nil
}

type performanceRouter struct{}

func (performanceRouter) Route(_ *echo.Group, _ echo.MiddlewareFunc) {}

func newPerformanceApp(tb testing.TB, count, statusLength int, logLevel logrus.Level) (*echo.Echo, *performanceStore) {
	tb.Helper()
	clock := &sessionClock{time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	s := &performanceStore{
		user:    users.User{ID: id.NewID(), Auth: users.Auth{Username: "viewer", DeviceToken: "device-a"}},
		members: make(map[id.ID]users.User),
	}
	s.user.FriendSince = make(map[string]time.Time)
	for group := range 3 {
		for n := range count {
			peerID := id.NewID()
			s.members[peerID] = users.User{
				ID: peerID, Auth: users.Auth{Username: fmt.Sprintf("peer-%d-%d", group, n)},
				Status:   strings.Repeat("s", statusLength),
				Location: &users.Location{Latitude: 52.23, Longitude: 21.01, LastUpdate: clock.Now()},
			}
			switch group {
			case 0:
				s.user.SubscribedUsers = append(s.user.SubscribedUsers, peerID)
				s.user.FriendSince[peerID.Hex()] = clock.Now()
			case 1:
				s.incoming = append(s.incoming, peerID)
			case 2:
				s.outgoing = append(s.outgoing, peerID)
			}
		}
	}
	j := jwt.NewJWT(clock, []byte("performance-test-secret"), 15*time.Minute, time.Hour)
	token, _, err := j.GenerateTokens(s.user.Auth.Username, s.user.ID)
	if err != nil {
		tb.Fatal(err)
	}
	s.user.Auth.Token = token
	log := logrus.New()
	log.SetOutput(io.Discard)
	log.SetLevel(logLevel)
	e := webapi.NewEcho("", validator.New(), j, s, webapi.EchoRouters{
		Swagger:    func(c *echo.Context) error { return c.NoContent(204) },
		AuthRouter: performanceRouter{}, MeRouter: me.NewMux(s, clock),
	}, log, false)
	return e, s
}

func performanceRequest(ctx context.Context, e *echo.Echo, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if token != "" {
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	}
	response := httptest.NewRecorder()
	e.ServeHTTP(response, req)
	return response
}

func TestFriendListQueriesRemainBatched(t *testing.T) {
	for _, count := range []int{0, 10, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			e, store := newPerformanceApp(t, count, 32, logrus.InfoLevel)
			response := performanceRequest(t.Context(), e, http.MethodGet, "/me/friends", store.user.Auth.Token, "")
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			var entries []json.RawMessage
			if err := json.Unmarshal(response.Body.Bytes(), &entries); err != nil {
				t.Fatal(err)
			}
			if len(entries) != 3*count || store.sessionReads.Load() != 1 || store.userReads.Load() != 1 || store.batchReads.Load() != 3 || store.listReads.Load() != 2 {
				t.Fatalf("entries=%d, session reads=%d, full user reads=%d, batch reads=%d, pending reads=%d", len(entries), store.sessionReads.Load(), store.userReads.Load(), store.batchReads.Load(), store.listReads.Load())
			}
		})
	}
}

type canceledFriendsStore struct {
	*performanceStore
	started chan struct{}
	done    chan error
}

func (s *canceledFriendsStore) GetUsers(ctx context.Context, _ []id.ID) ([]users.User, error) {
	s.started <- struct{}{}
	<-ctx.Done()
	s.done <- ctx.Err()
	return nil, fmt.Errorf("friends load canceled: %w", ctx.Err())
}

func (s *canceledFriendsStore) GetPendingOutgoingFriendRequestUserIDs(ctx context.Context, _ id.ID) ([]id.ID, error) {
	s.started <- struct{}{}
	<-ctx.Done()
	s.done <- ctx.Err()
	return nil, fmt.Errorf("outgoing load canceled: %w", ctx.Err())
}

func (s *canceledFriendsStore) GetPendingIncomingFriendRequestUserIDs(ctx context.Context, _ id.ID) ([]id.ID, error) {
	for range 2 {
		select {
		case <-s.started:
		case <-ctx.Done():
			return nil, fmt.Errorf("incoming load canceled: %w", ctx.Err())
		}
	}
	return nil, errors.New("pending read failed")
}

func TestFriendListFailureCancelsOutstandingLoads(t *testing.T) {
	e, store := newPerformanceApp(t, 1, 0, logrus.InfoLevel)
	failing := &canceledFriendsStore{store, make(chan struct{}, 2), make(chan error, 2)}
	me.NewMux(failing, &sessionClock{time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}).Route(e.Group("/failure"), nil)
	// This route bypasses the production authentication middleware only to inject claims.
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("user", jwt.SignedToken{ID: store.user.ID.Hex()})
			return next(c)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	response := performanceRequest(ctx, e, http.MethodGet, "/failure/friends", "", "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", response.Code)
	}
	for range 2 {
		select {
		case err := <-failing.done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("outstanding load error = %v, want immediate cancellation", err)
			}
		case <-ctx.Done():
			t.Fatal("outstanding loads did not stop when the handler returned")
		}
	}
}

func BenchmarkHTTPFriends(b *testing.B) {
	for _, count := range []int{0, 10, 100, 1000} {
		for _, statusLength := range []int{32, 1024} {
			b.Run(fmt.Sprintf("each_group=%d/status=%d", count, statusLength), func(b *testing.B) {
				e, store := newPerformanceApp(b, count, statusLength, logrus.InfoLevel)
				var responseBytes int
				b.ReportAllocs()
				for b.Loop() {
					response := performanceRequest(b.Context(), e, http.MethodGet, "/me/friends", store.user.Auth.Token, "")
					if response.Code != http.StatusOK {
						b.Fatalf("status = %d", response.Code)
					}
					responseBytes = response.Body.Len()
				}
				b.ReportMetric(float64(responseBytes), "response_B")
			})
		}
	}
}

func BenchmarkHTTPLocation(b *testing.B) {
	e, store := newPerformanceApp(b, 0, 0, logrus.InfoLevel)
	body := `{"latitude":52.23,"longitude":21.01,"last_update":1791028800000}`
	b.ReportAllocs()
	for b.Loop() {
		response := performanceRequest(b.Context(), e, http.MethodPut, "/me/location", store.user.Auth.Token, body)
		if response.Code != http.StatusNoContent {
			b.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
	}
}

func BenchmarkHTTPFriendsPausedLists(b *testing.B) {
	for _, pausedCount := range []int{0, 1000} {
		b.Run(fmt.Sprintf("friends=1000/paused_per_friend=%d", pausedCount), func(b *testing.B) {
			e, store := newPerformanceApp(b, 1000, 32, logrus.InfoLevel)
			paused := make([]id.ID, pausedCount)
			for i := range paused {
				paused[i] = id.NewID()
			}
			for peerID, peer := range store.members {
				peer.PausedUsers = paused
				store.members[peerID] = peer
			}
			b.ReportAllocs()
			for b.Loop() {
				response := performanceRequest(b.Context(), e, http.MethodGet, "/me/friends", store.user.Auth.Token, "")
				if response.Code != http.StatusOK {
					b.Fatalf("status = %d", response.Code)
				}
			}
		})
	}
}

func BenchmarkHTTPFriendsEscapedStatus(b *testing.B) {
	e, store := newPerformanceApp(b, 1000, 0, logrus.InfoLevel)
	status := strings.Repeat("<", 1024)
	for peerID, peer := range store.members {
		peer.Status = status
		store.members[peerID] = peer
	}
	var responseBytes int
	b.ReportAllocs()
	for b.Loop() {
		response := performanceRequest(b.Context(), e, http.MethodGet, "/me/friends", store.user.Auth.Token, "")
		if response.Code != http.StatusOK {
			b.Fatalf("status = %d", response.Code)
		}
		responseBytes = response.Body.Len()
	}
	b.ReportMetric(float64(responseBytes), "response_B")
}

func BenchmarkHTTPHealth(b *testing.B) {
	for _, level := range []logrus.Level{logrus.InfoLevel, logrus.ErrorLevel} {
		for _, size := range []int{0, 1024, webapi.MaxRequestBodyBytes} {
			b.Run(fmt.Sprintf("log=%s/body=%d", level, size), func(b *testing.B) {
				e, _ := newPerformanceApp(b, 0, 0, level)
				body := strings.Repeat(" ", size)
				b.ReportAllocs()
				for b.Loop() {
					response := performanceRequest(b.Context(), e, http.MethodGet, "/health", "", body)
					if response.Code != http.StatusOK {
						b.Fatalf("status = %d", response.Code)
					}
				}
			})
		}
	}
}
