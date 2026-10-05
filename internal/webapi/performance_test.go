package webapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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

type friendListPage struct {
	Items      []map[string]json.RawMessage `json:"items"`
	NextCursor *string                      `json:"next_cursor"`
}

func fixtureFriendPage(viewer users.User, members map[id.ID]users.User, incoming, outgoing []id.ID, query users.FriendPageQuery) users.FriendPage {
	ids := viewer.SubscribedUsers
	switch query.State {
	case users.FriendListAccepted:
	case users.FriendListIncoming:
		ids = incoming
	case users.FriendListOutgoing:
		ids = outgoing
	}
	ids = slices.Clone(ids)
	slices.SortFunc(ids, func(a, b id.ID) int { return bytes.Compare(a[:], b[:]) })
	ids = slices.DeleteFunc(ids, func(peer id.ID) bool { return bytes.Compare(peer[:], query.After[:]) <= 0 })
	page := users.FriendPage{Entries: make([]users.FriendEntry, 0)}
	if len(ids) > query.Limit {
		ids = ids[:query.Limit]
		page.NextID = ids[len(ids)-1]
	}
	for _, peer := range ids {
		entry := users.FriendEntry{User: members[peer]}
		if query.State == users.FriendListAccepted {
			entry.FriendSince = viewer.FriendSinceFor(peer)
		}
		page.Entries = append(page.Entries, entry)
	}
	return page
}

func (s *performanceStore) GetFriendPage(_ context.Context, _ id.ID, query users.FriendPageQuery) (users.FriendPage, error) {
	s.listReads.Add(1)
	return fixtureFriendPage(s.user, s.members, s.incoming, s.outgoing, query), nil
}

func TestFriendListQueriesRemainBatched(t *testing.T) {
	for _, count := range []int{0, 10, 1000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			e, store := newPerformanceApp(t, count, 32, logrus.InfoLevel)
			response := performanceRequest(t.Context(), e, http.MethodGet, "/me/friends", store.user.Auth.Token, "")
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", response.Code, response.Body.String())
			}
			var page friendListPage
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != min(count, users.MaxFriendPageSize) || store.sessionReads.Load() != 1 ||
				store.userReads.Load() != 0 || store.batchReads.Load() != 0 || store.listReads.Load() != 1 {
				t.Fatalf("entries=%d, session reads=%d, full user reads=%d, batch reads=%d, page reads=%d",
					len(page.Items), store.sessionReads.Load(), store.userReads.Load(), store.batchReads.Load(), store.listReads.Load())
			}
		})
	}
}

func TestFriendListPreservesGroupsWithEmptyResults(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		accepted, incoming, outgoing int
	}{
		{name: "all groups", accepted: 2, incoming: 1, outgoing: 2},
		{name: "accepted empty", incoming: 1, outgoing: 2},
		{name: "incoming empty", accepted: 2, outgoing: 1},
		{name: "outgoing empty", accepted: 1, incoming: 2},
		{name: "accepted only", accepted: 2},
		{name: "incoming only", incoming: 2},
		{name: "outgoing only", outgoing: 2},
		{name: "all empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, store := newPerformanceApp(t, 2, 32, logrus.InfoLevel)
			store.user.SubscribedUsers = store.user.SubscribedUsers[:tc.accepted]
			store.incoming = store.incoming[:tc.incoming]
			store.outgoing = store.outgoing[:tc.outgoing]
			if tc.accepted > 1 {
				pausedID := store.user.SubscribedUsers[1]
				paused := store.members[pausedID]
				paused.PausedUsers = []id.ID{store.user.ID}
				store.members[pausedID] = paused
			}

			entries := make([]map[string]json.RawMessage, 0)
			for _, state := range []string{"accepted", "pending_incoming", "pending_outgoing"} {
				response := performanceRequest(t.Context(), e, http.MethodGet, "/me/friends?state="+state, store.user.Auth.Token, "")
				if response.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", response.Code, response.Body.String())
				}
				var page friendListPage
				if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				if page.Items == nil || page.NextCursor != nil {
					t.Fatal("small page must contain an array and no continuation")
				}
				entries = append(entries, page.Items...)
			}
			if len(entries) != tc.accepted+tc.incoming+tc.outgoing {
				t.Fatal("relationship counts changed")
			}

			index := 0
			for _, group := range []struct {
				ids   []id.ID
				state string
			}{
				{store.user.SubscribedUsers, "accepted"},
				{store.incoming, "pending_incoming"},
				{store.outgoing, "pending_outgoing"},
			} {
				for i, userID := range group.ids {
					user := store.members[userID]
					entry := entries[index]
					var username string
					if err := json.Unmarshal(entry["username"], &username); err != nil {
						t.Fatal(err)
					}
					if username != user.Auth.Username {
						t.Fatalf("entry %d username = %q, want %q", index, username, user.Auth.Username)
					}
					assertFriendVisibility(t, entry, group.state, user.Status, group.state == "accepted" && i == 0)
					if group.state == "accepted" {
						var since int64
						if err := json.Unmarshal(entry["friend_since"], &since); err != nil {
							t.Fatal(err)
						}
						if since != store.user.FriendSince[userID.Hex()].UnixMilli() {
							t.Fatalf("entry %d lost its friendship timestamp", index)
						}
					} else if string(entry["friend_since"]) != "null" {
						t.Fatalf("pending entry %d exposed a friendship timestamp", index)
					}
					index++
				}
			}
		})
	}
}

type canceledFriendsStore struct {
	*performanceStore
	ctx context.Context
}

func (s *canceledFriendsStore) GetFriendPage(ctx context.Context, _ id.ID, _ users.FriendPageQuery) (users.FriendPage, error) {
	s.ctx = ctx
	return users.FriendPage{}, errors.New("friend page failed")
}

func TestFriendListFailureCancelsOutstandingLoads(t *testing.T) {
	e, store := newPerformanceApp(t, 1, 0, logrus.InfoLevel)
	failing := &canceledFriendsStore{performanceStore: store}
	me.NewMux(failing, &sessionClock{time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}).Route(e.Group("/failure"), nil)
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error { c.Set("user", jwt.SignedToken{ID: store.user.ID.Hex()}); return next(c) }
	})
	response := performanceRequest(t.Context(), e, http.MethodGet, "/failure/friends", "", "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", response.Code)
	}
	if failing.ctx == nil || !errors.Is(failing.ctx.Err(), context.Canceled) {
		t.Fatal("failed page did not release its request context")
	}
}

func TestHTTPFriendPageTraversal(t *testing.T) {
	e, store := newPerformanceApp(t, 123, 1024, logrus.InfoLevel)
	for _, state := range []string{"accepted", "pending_incoming", "pending_outgoing"} {
		cursor := ""
		seen := make(map[string]bool)
		for {
			response := performanceRequest(t.Context(), e, http.MethodGet,
				"/me/friends?state="+state+"&limit=50&cursor="+url.QueryEscape(cursor), store.user.Auth.Token, "")
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d", response.Code)
			}
			var page friendListPage
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) > users.MaxFriendPageSize {
				t.Fatal("page exceeded maximum size")
			}
			for _, entry := range page.Items {
				var username string
				if err := json.Unmarshal(entry["username"], &username); err != nil {
					t.Fatal(err)
				}
				if seen[username] {
					t.Fatal("duplicate entry across pages")
				}
				seen[username] = true
				assertFriendVisibility(t, entry, state, strings.Repeat("s", 1024), state == "accepted")
			}
			if page.NextCursor == nil {
				break
			}
			if *page.NextCursor == cursor {
				t.Fatal("cursor did not advance")
			}
			cursor = *page.NextCursor
		}
		if len(seen) != 123 {
			t.Fatalf("traversal returned %d entries", len(seen))
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
