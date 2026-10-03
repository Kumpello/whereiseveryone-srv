package me

import (
	"context"
	"errors"
	"fmt"
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
	"whereiseveryone/pkg/timer"
)

type statusStore struct {
	users.Adapter
	ctx context.Context
	err error
}

type locationStore struct {
	users.Adapter
	ctx      context.Context
	userID   id.ID
	location users.Location
	err      error
}

func (s *locationStore) UpdateLocation(ctx context.Context, userID id.ID, location users.Location) error {
	s.ctx, s.userID, s.location = ctx, userID, location
	return s.err
}

type locationClock struct{ now time.Time }

func (c locationClock) Now() time.Time { return c.now }

func TestUpdateLocationValidatesTimestampBeforePersistence(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 123000000, time.UTC)
	millis := func(offset time.Duration) string { return fmt.Sprint(now.Add(offset).UnixMilli()) }
	for _, tc := range []struct {
		name      string
		timestamp string
		wantCode  int
		wantTime  time.Time
		storeErr  error
	}{
		{name: "current fix", timestamp: millis(0), wantCode: 204, wantTime: now},
		{name: "delayed fix", timestamp: millis(-time.Hour), wantCode: 204, wantTime: now.Add(-time.Hour)},
		{name: "oldest accepted fix", timestamp: millis(-24 * time.Hour), wantCode: 204, wantTime: now.Add(-24 * time.Hour)},
		{name: "maximum clock skew is capped", timestamp: millis(5 * time.Minute), wantCode: 204, wantTime: now},
		{name: "small clock skew is capped", timestamp: millis(time.Millisecond), wantCode: 204, wantTime: now},
		{name: "too old", timestamp: millis(-24*time.Hour - time.Millisecond), wantCode: 400},
		{name: "too far ahead", timestamp: millis(5*time.Minute + time.Millisecond), wantCode: 400},
		{name: "missing", wantCode: 400},
		{name: "null", timestamp: "null", wantCode: 400},
		{name: "zero", timestamp: "0", wantCode: 400},
		{name: "negative", timestamp: "-1", wantCode: 400},
		{name: "seconds instead of milliseconds", timestamp: fmt.Sprint(now.Unix()), wantCode: 400},
		{name: "microseconds instead of milliseconds", timestamp: fmt.Sprint(now.UnixMicro()), wantCode: 400},
		{name: "fractional number", timestamp: millis(0) + ".5", wantCode: 400},
		{name: "quoted number", timestamp: `"` + millis(0) + `"`, wantCode: 400},
		{name: "boolean", timestamp: "true", wantCode: 400},
		{name: "object", timestamp: "{}", wantCode: 400},
		{name: "array", timestamp: "[]", wantCode: 400},
		{name: "maximum integer", timestamp: "9223372036854775807", wantCode: 400},
		{name: "minimum integer", timestamp: "-9223372036854775808", wantCode: 400},
		{name: "integer overflow", timestamp: "9223372036854775808", wantCode: 400},
		{name: "database failure", timestamp: millis(0), wantCode: 500, wantTime: now, storeErr: errors.New("database failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &locationStore{err: tc.storeErr}
			router := NewMux(store, locationClock{now})
			e := echo.New()
			e.Validator = handlerValidator{validator.New()}
			body := `{"latitude":52.23,"longitude":21.01`
			if tc.timestamp != "" {
				body += `,"last_update":` + tc.timestamp
			}
			body += "}"
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/me/location", strings.NewReader(body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			response := httptest.NewRecorder()
			c := e.NewContext(req, response)
			userID := id.NewID()
			c.Set("user", jwt.SignedToken{ID: userID.Hex()})
			if err := router.updateLocation(c); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.wantCode {
				t.Fatalf("HTTP status = %d, want %d; body = %s", response.Code, tc.wantCode, response.Body.String())
			}
			if tc.wantCode == http.StatusBadRequest {
				if store.ctx != nil {
					t.Fatal("invalid timestamp reached persistence")
				}
				return
			}
			if store.userID != userID || !store.location.LastUpdate.Equal(tc.wantTime) ||
				store.location.Latitude != 52.23 || store.location.Longitude != 21.01 {
				t.Fatalf("stored user = %v, location = %+v; expected time = %v", store.userID, store.location, tc.wantTime)
			}
			if store.ctx == nil || !errors.Is(store.ctx.Err(), context.Canceled) {
				t.Fatal("handler returned without canceling the database context")
			}
		})
	}
}

func (s *statusStore) UpdateStatus(ctx context.Context, _ id.ID, _ string) error {
	s.ctx = ctx
	return s.err
}

type handlerValidator struct{ validate *validator.Validate }

func (v handlerValidator) Validate(value any) error { return v.validate.Struct(value) }

func TestUpdateStatusCancelsDatabaseContext(t *testing.T) {
	for _, tc := range []struct {
		name     string
		storeErr error
		wantCode int
	}{
		{name: "success", wantCode: http.StatusNoContent},
		{name: "database failure", storeErr: errors.New("database failure"), wantCode: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &statusStore{err: tc.storeErr}
			router := NewMux(store, timer.NewUTCTimer())
			e := echo.New()
			e.Validator = handlerValidator{validator.New()}
			req := httptest.NewRequest(http.MethodPut, "/me/status", strings.NewReader(`{"status":"hello"}`)).WithContext(parent)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			response := httptest.NewRecorder()
			c := e.NewContext(req, response)
			c.Set("user", jwt.SignedToken{ID: id.NewID().Hex()})
			if err := router.updateStatus(c); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.wantCode {
				t.Fatalf("HTTP status = %d, want %d", response.Code, tc.wantCode)
			}
			if store.ctx == nil || !errors.Is(store.ctx.Err(), context.Canceled) {
				t.Fatal("handler returned without canceling the database context")
			}
			if parent.Err() != nil {
				t.Fatal("handler canceled the parent request")
			}
		})
	}
}
