package binder

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"

	"whereiseveryone/pkg/jwt"
)

type testValidator struct{ validate *validator.Validate }

func (v testValidator) Validate(value any) error { return v.validate.Struct(value) }

func TestBindRequestCancelsOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		requireAuth bool
		claims      *jwt.SignedToken
		wantCode    int
	}{
		{name: "missing authentication", body: `{"name":"alice"}`, requireAuth: true, wantCode: http.StatusForbidden},
		{name: "invalid user ID", body: `{"name":"alice"}`, requireAuth: true, claims: &jwt.SignedToken{ID: "invalid"}, wantCode: http.StatusBadRequest},
		{name: "invalid JSON", body: `{`, wantCode: http.StatusBadRequest},
		{name: "validation failure", body: `{}`, wantCode: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			e := echo.New()
			e.Validator = testValidator{validator.New()}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body)).WithContext(parent)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			c := e.NewContext(req, httptest.NewRecorder())
			if tc.claims != nil {
				c.Set("user", *tc.claims)
			}
			type body struct {
				Name string `json:"name" validate:"required"`
			}
			request, err := BindRequest[body](c, tc.requireAuth)
			if err == nil || err.Code != tc.wantCode {
				t.Fatalf("binding error = %v, want HTTP %d", err, tc.wantCode)
			}
			if !errors.Is(request.Context().Err(), context.Canceled) {
				t.Fatalf("failed binding left context active: %v", request.Context().Err())
			}
			if parent.Err() != nil {
				t.Fatal("binding canceled the parent request")
			}
		})
	}
}

func TestBindRequestTransfersCancellationToCaller(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := echo.New()
	e.Validator = testValidator{validator.New()}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`)).WithContext(parent)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	request, err := BindRequest[EmptyBody](e.NewContext(req, httptest.NewRecorder()), false)
	if err != nil {
		t.Fatal(err)
	}
	if request.Context().Err() != nil {
		t.Fatal("successful binding canceled context before the caller could use it")
	}
	if _, ok := request.Context().Deadline(); !ok {
		t.Fatal("bound context has no deadline")
	}
	func() {
		defer request.Cancel()
	}()
	if !errors.Is(request.Context().Err(), context.Canceled) {
		t.Fatalf("deferred cancellation did not cancel context: %v", request.Context().Err())
	}
	request.Cancel() // Repeated cancellation is safe.
	if parent.Err() != nil {
		t.Fatal("caller cleanup canceled the parent request")
	}
}
