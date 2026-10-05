package me

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"

	"whereiseveryone/internal/users"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
	"whereiseveryone/pkg/timer"
)

type pageStore struct {
	users.Adapter
	reads int
	query users.FriendPageQuery
}

func (s *pageStore) GetFriendPage(_ context.Context, _ id.ID, query users.FriendPageQuery) (users.FriendPage, error) {
	s.reads++
	s.query = query
	return users.FriendPage{Entries: []users.FriendEntry{}}, nil
}

func TestHTTPFriendPageParameters(t *testing.T) {
	viewer, after := id.NewID(), id.NewID()
	accepted, err := nextFriendCursor(viewer, users.FriendListAccepted, after)
	if err != nil {
		t.Fatal(err)
	}
	otherViewer, err := nextFriendCursor(id.NewID(), users.FriendListAccepted, after)
	if err != nil {
		t.Fatal(err)
	}
	otherList, err := nextFriendCursor(viewer, users.FriendListIncoming, after)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, query string
		code, limit int
		state       users.FriendListState
		after       id.ID
	}{
		{"default", "", 200, 50, users.FriendListAccepted, id.ID{}},
		{"minimum", "?limit=1", 200, 1, users.FriendListAccepted, id.ID{}},
		{"maximum", "?limit=50&state=pending_outgoing", 200, 50, users.FriendListOutgoing, id.ID{}},
		{"continuation", "?cursor=" + url.QueryEscape(*accepted), 200, 50, users.FriendListAccepted, after},
		{"zero", "?limit=0", 400, 0, "", id.ID{}},
		{"over maximum", "?limit=51", 400, 0, "", id.ID{}},
		{"negative", "?limit=-1", 400, 0, "", id.ID{}},
		{"fractional", "?limit=2.5", 400, 0, "", id.ID{}},
		{"nonnumeric", "?limit=abc", 400, 0, "", id.ID{}},
		{"unknown state", "?state=all", 400, 0, "", id.ID{}},
		{"malformed cursor", "?cursor=not-json", 400, 0, "", id.ID{}},
		{"oversized cursor", "?cursor=" + strings.Repeat("a", 257), 400, 0, "", id.ID{}},
		{"different viewer", "?cursor=" + url.QueryEscape(*otherViewer), 400, 0, "", id.ID{}},
		{"different list", "?cursor=" + url.QueryEscape(*otherList), 400, 0, "", id.ID{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &pageStore{}
			e := echo.New()
			e.Validator = handlerValidator{validator.New()}
			response := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/me/friends"+tc.query, nil), response)
			c.Set("user", jwt.SignedToken{ID: viewer.Hex()})
			if err := NewMux(store, timer.NewUTCTimer()).getFriends(c); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.code {
				t.Fatalf("code=%d want=%d body=%s", response.Code, tc.code, response.Body.String())
			}
			if tc.code == 400 {
				if store.reads != 0 {
					t.Fatal("invalid page parameters reached database")
				}
				return
			}
			if store.reads != 1 || store.query != (users.FriendPageQuery{State: tc.state, Limit: tc.limit, After: tc.after}) {
				t.Fatalf("query=%+v reads=%d", store.query, store.reads)
			}
			var page getFriendsResponse
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			if page.Items == nil || page.NextCursor != nil {
				t.Fatal("empty list must serialize as items:[] and next_cursor:null")
			}
		})
	}
}

type relationshipFailureStore struct {
	users.Adapter
	peer users.User
	err  error
}

func (s *relationshipFailureStore) GetUserByUsername(context.Context, string) (users.User, error) {
	return s.peer, nil
}
func (s *relationshipFailureStore) GetUser(context.Context, id.ID) (users.User, error) {
	return users.User{}, nil
}
func (s *relationshipFailureStore) SendFriendRequest(context.Context, id.ID, id.ID) error {
	return s.err
}
func (s *relationshipFailureStore) AcceptFriendRequest(context.Context, id.ID, id.ID) error {
	return s.err
}

func TestHTTPRelationshipLimitsReturnConflict(t *testing.T) {
	for _, accept := range []bool{false, true} {
		for _, tc := range []struct {
			err  error
			code int
		}{
			{users.ErrFriendsLimit, 409}, {users.ErrIncomingRequestsLimit, 409}, {users.ErrOutgoingRequestsLimit, 409},
			{users.ErrFriendRequestNotExists, 404}, {errors.New("database failure"), 500},
		} {
			store := &relationshipFailureStore{peer: users.User{ID: id.NewID()}, err: tc.err}
			e := echo.New()
			e.Validator = handlerValidator{validator.New()}
			response := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/me/friend", strings.NewReader(`{"username":"peer"}`))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			c := e.NewContext(req, response)
			c.Set("user", jwt.SignedToken{ID: id.NewID().Hex()})
			router := NewMux(store, timer.NewUTCTimer())
			var err error
			if accept {
				err = router.acceptFriend(c)
			} else {
				err = router.befriend(c)
			}
			if err != nil || response.Code != tc.code {
				t.Fatalf("accept=%t err=%v code=%d want=%d", accept, err, response.Code, tc.code)
			}
		}
	}
}
