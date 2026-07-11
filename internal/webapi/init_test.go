package webapi

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"whereiseveryone/pkg/id"
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

type staticTimer struct {
	now time.Time
}

func (s staticTimer) Now() time.Time {
	return s.now
}

func TestJWTValidatorMapsExpiredTokenToAppError(t *testing.T) {
	baseTime := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	j := jwt.NewJWT(staticTimer{now: baseTime}, []byte("secret"), time.Minute, time.Hour)

	userID := id.NewID()
	token, _, err := j.GenerateTokens("user", userID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	expiredJWT := jwt.NewJWT(staticTimer{now: baseTime.Add(2 * time.Minute)}, []byte("secret"), time.Minute, time.Hour)
	_, err = expiredJWT.ValidateToken(token)
	if !errors.Is(err, jwt.ErrTokenExpired) {
		t.Fatalf("expected expired token error, got %v", err)
	}
}
