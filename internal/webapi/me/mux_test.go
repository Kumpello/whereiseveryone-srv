package me

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
