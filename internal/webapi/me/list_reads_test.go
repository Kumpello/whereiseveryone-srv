package me

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"

	"whereiseveryone/internal/users"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
	"whereiseveryone/pkg/timer"
)

type projectedListStore struct {
	users.Adapter
	page   users.FriendPage
	names  []users.UserName
	err    error
	viewer id.ID
	reads  int
}

func (s *projectedListStore) GetFriendPage(_ context.Context, viewer id.ID, _ users.FriendPageQuery) (users.FriendPage, error) {
	s.viewer, s.reads = viewer, s.reads+1
	return s.page, s.err
}

func (s *projectedListStore) GetPausedUsers(_ context.Context, viewer id.ID) ([]users.UserName, error) {
	s.viewer, s.reads = viewer, s.reads+1
	return s.names, s.err
}

func TestHTTPFriendsUseProjectedVisibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   users.FriendListState
		visible bool
		located bool
	}{
		{"visible accepted", users.FriendListAccepted, true, true},
		{"paused accepted", users.FriendListAccepted, false, true},
		{"no accepted fix", users.FriendListAccepted, true, false},
		{"incoming defensive privacy", users.FriendListIncoming, true, true},
		{"outgoing defensive privacy", users.FriendListOutgoing, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viewer := id.NewID()
			peer := users.FriendPeer{
				UserName: users.UserName{ID: id.NewID(), Username: "peer"},
				Status:   "friend status", LocationVisible: tc.visible,
			}
			if tc.located {
				peer.Location = &users.Location{Latitude: 52, Longitude: 21}
			}
			store := &projectedListStore{page: users.FriendPage{Entries: []users.FriendEntry{{Peer: peer}}}}
			e := echo.New()
			e.Validator = handlerValidator{validator.New()}
			response := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/me/friends?state="+string(tc.state), nil)
			c := e.NewContext(req, response)
			c.Set("user", jwt.SignedToken{ID: viewer.Hex()})
			if err := NewMux(store, timer.NewUTCTimer()).getFriends(c); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || store.viewer != viewer || store.reads != 1 {
				t.Fatalf("status=%d viewer=%s reads=%d", response.Code, store.viewer, store.reads)
			}
			var page struct {
				Items []map[string]json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 {
				t.Fatalf("unexpected response: %s", response.Body.String())
			}
			entry := page.Items[0]
			_, hasLocation := entry["location"]
			_, hasStatus := entry["status"]
			if hasLocation != (tc.state == users.FriendListAccepted && tc.visible && tc.located) ||
				hasStatus != (tc.state == users.FriendListAccepted) {
				t.Fatalf("projection privacy changed: %s", response.Body.String())
			}
			for _, field := range []string{"location_visible", "paused_users", "auth", "_id"} {
				if _, ok := entry[field]; ok {
					t.Fatalf("internal field %s reached response", field)
				}
			}
		})
	}
}

func TestHTTPPausedSharingUsesOnlyProjectedNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []users.UserName
		err   error
		code  int
	}{
		{"empty", nil, nil, http.StatusOK},
		{"names", []users.UserName{{ID: id.NewID(), Username: "peer"}}, nil, http.StatusOK},
		{"query failure", nil, errors.New("database unavailable"), http.StatusInternalServerError},
		{"cancellation", nil, context.Canceled, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			viewer := id.NewID()
			store := &projectedListStore{names: tc.names, err: tc.err}
			e := echo.New()
			e.Validator = handlerValidator{validator.New()}
			response := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/me/sharing", nil), response)
			c.Set("user", jwt.SignedToken{ID: viewer.Hex()})
			if err := NewMux(store, timer.NewUTCTimer()).getPaused(c); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.code || store.viewer != viewer || store.reads != 1 {
				t.Fatalf("status=%d viewer=%s reads=%d", response.Code, store.viewer, store.reads)
			}
			if tc.err != nil {
				return
			}
			var entries []map[string]string
			if err := json.Unmarshal(response.Body.Bytes(), &entries); err != nil {
				t.Fatal(err)
			}
			if entries == nil || len(entries) != len(tc.names) {
				t.Fatalf("unexpected paused list: %s", response.Body.String())
			}
			for i, entry := range entries {
				if len(entry) != 1 || entry["username"] != tc.names[i].Username {
					t.Fatalf("paused sharing returned more than usernames: %+v", entry)
				}
			}
		})
	}
}
