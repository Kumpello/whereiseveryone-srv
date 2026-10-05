package webapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"sync"
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

type friendPrivacyStore struct {
	*sessionStore
	friendsMu sync.Mutex
	members   map[id.ID]users.User
	incoming  []id.ID
	outgoing  []id.ID
}

func (s *friendPrivacyStore) GetFriendPage(_ context.Context, _ id.ID, query users.FriendPageQuery) (users.FriendPage, error) {
	s.friendsMu.Lock()
	defer s.friendsMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return fixtureFriendPage(s.user, s.members, s.incoming, s.outgoing, query), nil
}

func (s *friendPrivacyStore) GetUsers(_ context.Context, userIDs []id.ID) ([]users.User, error) {
	s.friendsMu.Lock()
	defer s.friendsMu.Unlock()
	result := make([]users.User, 0, len(userIDs))
	for _, userID := range userIDs {
		if user, exists := s.members[userID]; exists {
			result = append(result, user)
		}
	}
	return result, nil
}

func (s *friendPrivacyStore) GetUserByUsername(_ context.Context, username string) (users.User, error) {
	s.friendsMu.Lock()
	defer s.friendsMu.Unlock()
	for _, user := range s.members {
		if user.Auth.Username == username {
			return user, nil
		}
	}
	return users.User{}, users.ErrUserNotExists
}

func (s *friendPrivacyStore) GetPendingIncomingFriendRequestUserIDs(_ context.Context, _ id.ID) ([]id.ID, error) {
	s.friendsMu.Lock()
	defer s.friendsMu.Unlock()
	return slices.Clone(s.incoming), nil
}

func (s *friendPrivacyStore) GetPendingOutgoingFriendRequestUserIDs(_ context.Context, _ id.ID) ([]id.ID, error) {
	s.friendsMu.Lock()
	defer s.friendsMu.Unlock()
	return slices.Clone(s.outgoing), nil
}

func (s *friendPrivacyStore) SendFriendRequest(_ context.Context, _ id.ID, target id.ID) error {
	s.friendsMu.Lock()
	defer s.friendsMu.Unlock()
	s.outgoing = append(s.outgoing, target)
	return nil
}

func (s *friendPrivacyStore) AcceptFriendRequest(_ context.Context, _ id.ID, requester id.ID) error {
	s.friendsMu.Lock()
	defer s.friendsMu.Unlock()
	i := slices.Index(s.incoming, requester)
	if i < 0 {
		return users.ErrFriendRequestNotExists
	}
	s.incoming = slices.Delete(s.incoming, i, i+1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.user.SubscribedUsers = append(s.user.SubscribedUsers, requester)
	s.user.FriendSince[requester.Hex()] = s.clock.Now()
	return nil
}

func TestHTTPPendingFriendRequestsOmitStatus(t *testing.T) {
	a := newSessionApp(t)
	location := &users.Location{Latitude: 52, Longitude: 21, LastUpdate: a.clock.Now()}
	accepted := users.User{ID: id.NewID(), Auth: users.Auth{Username: "accepted"}, Status: "friend status", Location: location}
	empty := users.User{ID: id.NewID(), Auth: users.Auth{Username: "empty"}}
	incoming := users.User{ID: id.NewID(), Auth: users.Auth{Username: "incoming"}, Status: "incoming private status", Location: location}
	target := users.User{ID: id.NewID(), Auth: users.Auth{Username: "target"}, Status: "target private status", Location: location}
	a.store.user.SubscribedUsers = []id.ID{accepted.ID, empty.ID}
	a.store.user.FriendSince = map[string]time.Time{accepted.ID.Hex(): a.clock.Now().Add(-time.Hour)}
	store := &friendPrivacyStore{
		sessionStore: a.store,
		members:      map[id.ID]users.User{accepted.ID: accepted, empty.ID: empty, incoming.ID: incoming, target.ID: target},
		incoming:     []id.ID{incoming.ID},
	}
	log := logrus.New()
	log.SetOutput(io.Discard)
	j := jwt.NewJWT(a.clock, []byte("session-test-secret"), 15*time.Minute, 720*time.Hour)
	a.echo = webapi.NewEcho("", validator.New(), j, store, webapi.EchoRouters{
		Swagger:    func(c *echo.Context) error { return c.NoContent(http.StatusNoContent) },
		AuthRouter: &sessionProbe{}, MeRouter: me.NewMux(store, a.clock),
	}, log, false)
	token := a.store.user.Auth.Token
	response := a.request(http.MethodPost, "/me/friend", token, `{"username":"target"}`)
	if response.Code != http.StatusNoContent {
		t.Fatalf("send friend request status = %d: %s", response.Code, response.Body.String())
	}
	wantStates := map[string]string{
		accepted.Auth.Username: "accepted", empty.Auth.Username: "accepted",
		incoming.Auth.Username: "pending_incoming", target.Auth.Username: "pending_outgoing",
	}
	wantStatuses := map[string]string{accepted.Auth.Username: accepted.Status, empty.Auth.Username: ""}
	wantLocations := map[string]bool{accepted.Auth.Username: true}
	checkFriends := func() {
		t.Helper()
		entries := make([]map[string]json.RawMessage, 0)
		for _, state := range []string{"accepted", "pending_incoming", "pending_outgoing"} {
			listResponse := a.request(http.MethodGet, "/me/friends?state="+state, token, "")
			if listResponse.Code != http.StatusOK {
				t.Fatalf("list friends status = %d: %s", listResponse.Code, listResponse.Body.String())
			}
			var page friendListPage
			if err := json.Unmarshal(listResponse.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, page.Items...)
		}

		if len(entries) != len(wantStates) {
			t.Fatalf("friend count = %d, want %d", len(entries), len(wantStates))
		}
		seen := make(map[string]bool)
		for _, entry := range entries {
			var username string
			if err := json.Unmarshal(entry["username"], &username); err != nil {
				t.Fatal(err)
			}
			if _, exists := wantStates[username]; !exists || seen[username] {
				t.Fatalf("unexpected or duplicate entry for %q", username)
			}
			seen[username] = true
			assertFriendVisibility(t, entry, wantStates[username], wantStatuses[username], wantLocations[username])
		}
	}
	checkFriends()

	// Updating the target's status while the request remains pending must not
	// turn the friend list into a way to monitor that private status.
	store.friendsMu.Lock()
	target.Status = "updated private status"
	store.members[target.ID] = target
	store.friendsMu.Unlock()
	checkFriends()

	response = a.request(http.MethodPost, "/me/friend/accept", token, `{"username":"incoming"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("accept friend status = %d: %s", response.Code, response.Body.String())
	}
	var acceptedEntry map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &acceptedEntry); err != nil {
		t.Fatal(err)
	}
	assertFriendVisibility(t, acceptedEntry, "accepted", incoming.Status, false)
	wantStates[incoming.Auth.Username] = "accepted"
	wantStatuses[incoming.Auth.Username] = incoming.Status
	wantLocations[incoming.Auth.Username] = true
	checkFriends()
}

func assertFriendVisibility(t *testing.T, entry map[string]json.RawMessage, state, status string, location bool) {
	t.Helper()
	var actualState string
	if err := json.Unmarshal(entry["state"], &actualState); err != nil {
		t.Fatal(err)
	}
	if actualState != state {
		t.Fatalf("state = %q, want %q", actualState, state)
	}
	rawStatus, hasStatus := entry["status"]
	if state == "accepted" {
		if !hasStatus || string(rawStatus) == "null" {
			t.Fatal("accepted friend's status must be present as a string, including an empty status")
		}
		var actualStatus string
		if err := json.Unmarshal(rawStatus, &actualStatus); err != nil {
			t.Fatal(err)
		}
		if actualStatus != status {
			t.Fatalf("accepted status = %q, want %q", actualStatus, status)
		}
	} else if hasStatus {
		t.Fatalf("pending entry exposed a status field: %s", rawStatus)
	}
	if _, hasLocation := entry["location"]; hasLocation != location {
		t.Fatalf("location present = %t, want %t", hasLocation, location)
	}
}
