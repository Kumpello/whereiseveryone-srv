package webapi

import (
	"errors"
	"net/http"
	"testing"

	"whereiseveryone/pkg/jwt"
)

func TestJWTErrorToJSONError(t *testing.T) {
	t.Run("expired token returns unauthorized", func(t *testing.T) {
		err := JWTErrorToJSONError(jwt.ErrTokenExpired)
		if err == nil {
			t.Fatal("expected json error")
		}
		if err.Code != http.StatusUnauthorized {
			t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, err.Code)
		}
	})

	t.Run("invalid token returns forbidden", func(t *testing.T) {
		err := JWTErrorToJSONError(errors.New("invalid token"))
		if err == nil {
			t.Fatal("expected json error")
		}
		if err.Code != http.StatusForbidden {
			t.Fatalf("expected status %d, got %d", http.StatusForbidden, err.Code)
		}
	})
}
