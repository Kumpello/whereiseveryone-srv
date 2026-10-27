package me

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"
	"whereiseveryone/internal/users"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
)

type pointStore struct {
	users.Adapter
	owner   id.ID
	pointID id.ID
	point   users.PointOfInterest
	points  []users.PointOfInterest
	ctx     context.Context
	err     error
	page    users.FriendPage
}

func (s *pointStore) GetPointsOfInterest(ctx context.Context, owner id.ID) ([]users.PointOfInterest, error) {
	s.ctx, s.owner = ctx, owner
	return s.points, s.err
}

func (s *pointStore) CreatePointOfInterest(ctx context.Context, owner id.ID, point users.PointOfInterest) (users.PointOfInterest, error) {
	s.ctx, s.owner, s.point = ctx, owner, point
	point.ID, point.CreatedAt = id.NewID(), time.UnixMilli(1_800_000_000_000)
	return point, s.err
}

func (s *pointStore) UpdatePointOfInterest(ctx context.Context, owner id.ID, pointID id.ID, point users.PointOfInterest) (users.PointOfInterest, error) {
	s.ctx, s.owner, s.pointID, s.point = ctx, owner, pointID, point
	point.ID, point.CreatedAt = pointID, time.UnixMilli(1_800_000_000_000)
	return point, s.err
}

func (s *pointStore) DeletePointOfInterest(ctx context.Context, owner id.ID, pointID id.ID) error {
	s.ctx, s.owner, s.pointID = ctx, owner, pointID
	return s.err
}

func (s *pointStore) GetFriendPage(_ context.Context, _ id.ID, _ users.FriendPageQuery) (users.FriendPage, error) {
	return s.page, s.err
}

func pointContext(t *testing.T, method, body string, owner id.ID, pointID string) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	e.Validator = handlerValidator{validator.New()}
	req := httptest.NewRequestWithContext(t.Context(), method, "/me/points", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	response := httptest.NewRecorder()
	c := e.NewContext(req, response)
	c.Set("user", jwt.SignedToken{ID: owner.Hex(), UserName: "alice"})
	c.SetPath("/me/points/:id")
	c.SetPathValues(echo.PathValues{{Name: "id", Value: pointID}})
	return c, response
}

func TestPointInputValidationAndServerAssignedCreator(t *testing.T) {
	valid := `{"name":"  Bridge  ","description":"  Meet here  ","latitude":0,"longitude":0,"creator_username":"mallory","created_at":1,"id":"forged"}`
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"valid equator and ignored metadata", valid, 201},
		{"missing latitude", `{"name":"Point","longitude":0}`, 400},
		{"missing longitude", `{"name":"Point","latitude":0}`, 400},
		{"null latitude", `{"name":"Point","latitude":null,"longitude":0}`, 400},
		{"blank name", `{"name":"   ","latitude":0,"longitude":0}`, 400},
		{"long name", `{"name":"` + strings.Repeat("a", 65) + `","latitude":0,"longitude":0}`, 400},
		{"long description", `{"name":"Point","description":"` + strings.Repeat("a", 1001) + `","latitude":0,"longitude":0}`, 400},
		{"invalid latitude", `{"name":"Point","latitude":91,"longitude":0}`, 400},
		{"invalid longitude", `{"name":"Point","latitude":0,"longitude":181}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := id.NewID()
			store := &pointStore{}
			c, response := pointContext(t, http.MethodPost, tc.body, owner, "")
			if err := NewMux(store, locationClock{}).createPointOfInterest(c); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tc.want != 201 {
				if store.ctx != nil {
					t.Fatal("invalid point reached storage")
				}
				return
			}
			var point pointOfInterestDetail
			if err := json.Unmarshal(response.Body.Bytes(), &point); err != nil {
				t.Fatal(err)
			}
			if store.owner != owner || store.point.Name != "Bridge" || store.point.Description != "Meet here" ||
				point.CreatorUsername != "alice" || point.ID == "forged" || point.CreatedAt.Time().UnixMilli() != 1_800_000_000_000 {
				t.Fatalf("stored=%+v response=%+v", store.point, point)
			}
			if !errors.Is(store.ctx.Err(), context.Canceled) {
				t.Fatal("database context was not cancelled")
			}
		})
	}
}

func TestPointMutationErrorsAndOwnerScope(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		for _, tc := range []struct {
			name string
			err  error
			want int
		}{
			{"success", nil, 200},
			{"other owner or missing", users.ErrPointOfInterestNotFound, 404},
			{"database failure", errors.New("storage private detail"), 500},
		} {
			t.Run(method+tc.name, func(t *testing.T) {
				owner, pointID := id.NewID(), id.NewID()
				store := &pointStore{err: tc.err}
				c, response := pointContext(t, method, `{"name":"Updated","latitude":52,"longitude":21}`, owner, pointID.Hex())
				router := NewMux(store, locationClock{})
				var err error
				if method == http.MethodPut {
					err = router.updatePointOfInterest(c)
				} else {
					err = router.deletePointOfInterest(c)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := tc.want
				if method == http.MethodDelete && want == 200 {
					want = 204
				}
				if response.Code != want || store.owner != owner || store.pointID != pointID {
					t.Fatalf("status=%d owner=%s id=%s body=%s", response.Code, store.owner, store.pointID, response.Body.String())
				}
				if strings.Contains(response.Body.String(), "storage private detail") {
					t.Fatal("database error leaked")
				}
			})
		}
	}
	store := &pointStore{err: users.ErrPointsOfInterestLimit}
	c, response := pointContext(t, http.MethodPost, `{"name":"Sixth","latitude":52,"longitude":21}`, id.NewID(), "")
	if err := NewMux(store, locationClock{}).createPointOfInterest(c); err != nil {
		t.Fatal(err)
	}
	if response.Code != 409 {
		t.Fatalf("sixth point status=%d", response.Code)
	}
}

func TestPointListsAndAcceptedFriendPrivacy(t *testing.T) {
	point := users.PointOfInterest{ID: id.NewID(), Name: "Bridge", Latitude: 52, Longitude: 21, CreatedAt: time.UnixMilli(1_800_000_000_000)}
	store := &pointStore{points: []users.PointOfInterest{point}}
	owner := id.NewID()
	c, response := pointContext(t, http.MethodGet, "", owner, "")
	if err := NewMux(store, locationClock{}).getPointsOfInterest(c); err != nil {
		t.Fatal(err)
	}
	if response.Code != 200 || store.owner != owner || !strings.Contains(response.Body.String(), `"creator_username":"alice"`) {
		t.Fatalf("own points: %s", response.Body.String())
	}
	for _, state := range []users.FriendListState{users.FriendListAccepted, users.FriendListIncoming, users.FriendListOutgoing} {
		t.Run(string(state), func(t *testing.T) {
			store := &pointStore{page: users.FriendPage{Entries: []users.FriendEntry{{Peer: users.FriendPeer{
				UserName:        users.UserName{ID: id.NewID(), Username: "bob"},
				LocationVisible: false, PointsOfInterest: []users.PointOfInterest{point},
			}}}}}
			c, response := pointContext(t, http.MethodGet, "", owner, "")
			c.Request().URL.RawQuery = "state=" + string(state)
			if err := NewMux(store, locationClock{}).getFriends(c); err != nil {
				t.Fatal(err)
			}
			var page getFriendsResponse
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			got := page.Items[0]
			if state == users.FriendListAccepted {
				if len(got.PointsOfInterest) != 1 || got.PointsOfInterest[0].CreatorUsername != "bob" || got.Location != nil {
					t.Fatalf("paused accepted friend lost points or exposed location: %+v", got)
				}
			} else if len(got.PointsOfInterest) != 0 || strings.Contains(response.Body.String(), "Bridge") {
				t.Fatalf("pending friend leaked points: %s", response.Body.String())
			}
		})
	}
}
