package me

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"whereiseveryone/internal/users"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
)

func TestLocationCarModeBoolean(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		mode  bool
		code  int
	}{
		{"true", true, http.StatusNoContent},
		{"false", false, http.StatusNoContent},
		{`"true"`, false, http.StatusBadRequest},
		{"1", false, http.StatusBadRequest},
	} {
		t.Run(tc.value, func(t *testing.T) {
			store := &locationStore{}
			e := echo.New()
			e.Validator = handlerValidator{validator.New()}
			body := fmt.Sprintf(`{"latitude":52,"longitude":21,"speed":12.5,"car_mode":%s,"last_update":%d}`, tc.value, now.UnixMilli())
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/me/location", strings.NewReader(body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			response := httptest.NewRecorder()
			c := e.NewContext(req, response)
			c.Set("user", jwt.SignedToken{ID: id.NewID().Hex()})
			if err := NewMux(store, locationClock{now}).updateLocation(c); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.code {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.code, response.Body.String())
			}
			if tc.code == http.StatusNoContent &&
				(store.location.CarMode != tc.mode || store.location.Speed == nil || *store.location.Speed != 12.5) {
				t.Fatalf("stored location=%+v", store.location)
			}
			if tc.code == http.StatusBadRequest && store.ctx != nil {
				t.Fatal("invalid car mode reached persistence")
			}
		})
	}
}

func TestFriendLocationIncludesCarModeAndStationarySpeed(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		zero := 0.0
		body, err := json.Marshal(locationDetails{CarMode: enabled, Speed: &zero})
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["car_mode"]) != fmt.Sprint(enabled) || string(fields["speed"]) != "0" {
			t.Fatalf("car mode or zero speed missing: %s", body)
		}
	}
}

func TestLocationGPSCourseRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, enabled := range []bool{true, false} {
		for _, course := range []string{"missing", "null", "0", "90", "359.5"} {
			t.Run(fmt.Sprintf("car_mode=%t/bearing=%s", enabled, course), func(t *testing.T) {
				store := &locationStore{}
				e := echo.New()
				e.Validator = handlerValidator{validator.New()}
				body := fmt.Sprintf(`{"latitude":52,"longitude":21,"speed":15,"car_mode":%t,"last_update":%d`, enabled, now.UnixMilli())
				if course != "missing" {
					body += `,"bearing":` + course
				}
				body += "}"
				req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/me/location", strings.NewReader(body))
				req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				response := httptest.NewRecorder()
				c := e.NewContext(req, response)
				c.Set("user", jwt.SignedToken{ID: id.NewID().Hex()})
				if err := NewMux(store, locationClock{now}).updateLocation(c); err != nil {
					t.Fatal(err)
				}
				if response.Code != http.StatusNoContent {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				stored, err := bson.Marshal(store.location)
				if err != nil {
					t.Fatal(err)
				}
				var loaded users.Location
				if decodeErr := bson.Unmarshal(stored, &loaded); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				friendJSON, err := json.Marshal(locationDetails{Bearing: loaded.Bearing, CarMode: loaded.CarMode})
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if decodeErr := json.Unmarshal(friendJSON, &fields); decodeErr != nil {
					t.Fatal(decodeErr)
				}
				want := course
				if want == "missing" {
					want = "null"
				}
				if string(fields["bearing"]) != want || string(fields["car_mode"]) != fmt.Sprint(enabled) {
					t.Fatalf("course or car mode lost: %s", friendJSON)
				}
			})
		}
	}
}

func TestLocationSpeedRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, speed := range []string{"missing", "null", "0", "12.5"} {
		t.Run(speed, func(t *testing.T) {
			store := &locationStore{}
			e := echo.New()
			e.Validator = handlerValidator{validator.New()}
			body := fmt.Sprintf(`{"latitude":52,"longitude":21,"car_mode":true,"last_update":%d`, now.UnixMilli())
			if speed != "missing" {
				body += `,"speed":` + speed
			}
			body += "}"
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/me/location", strings.NewReader(body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			response := httptest.NewRecorder()
			c := e.NewContext(req, response)
			c.Set("user", jwt.SignedToken{ID: id.NewID().Hex()})
			if err := NewMux(store, locationClock{now}).updateLocation(c); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusNoContent {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			stored, err := bson.Marshal(store.location)
			if err != nil {
				t.Fatal(err)
			}
			var loaded users.Location
			if decodeErr := bson.Unmarshal(stored, &loaded); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if bson.Raw(stored).Lookup("has_speed").Type != 0 {
				t.Fatal("redundant speed presence flag persisted")
			}
			friendJSON, err := json.Marshal(locationDetails{Speed: loaded.Speed, CarMode: loaded.CarMode})
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if decodeErr := json.Unmarshal(friendJSON, &fields); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			want := speed
			if want == "missing" {
				want = "null"
			}
			if string(fields["speed"]) != want {
				t.Fatalf("speed lost: %s", friendJSON)
			}
		})
	}
}
