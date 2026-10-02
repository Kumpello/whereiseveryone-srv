package jwt

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	jwtgo "github.com/golang-jwt/jwt/v5"
	"whereiseveryone/pkg/id"
)

type fixedTimer struct{ now time.Time }

func (c fixedTimer) Now() time.Time { return c.now }

func TestTokenPurposes(t *testing.T) {
	now := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	j := NewJWT(fixedTimer{now}, []byte("test-secret"), 15*time.Minute, 720*time.Hour)
	userID := id.NewID()
	access, refresh, err := j.GenerateTokens("alice", userID)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		token    string
		validate func(string) (SignedToken, error)
		purpose  string
		validity time.Duration
		wantErr  error
	}{
		{"access accepted by access validator", access, j.ValidateAccessToken, accessPurpose, 15 * time.Minute, nil},
		{"refresh accepted by refresh validator", refresh, j.ValidateRefreshToken, refreshPurpose, 720 * time.Hour, nil},
		{"refresh rejected by access validator", refresh, j.ValidateAccessToken, "", 0, ErrInvalidTokenPurpose},
		{"access rejected by refresh validator", access, j.ValidateRefreshToken, "", 0, ErrInvalidTokenPurpose},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := tc.validate(tc.token)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if claims.ID != userID.Hex() || claims.UserName != "alice" || claims.Purpose != tc.purpose {
				t.Fatalf("unexpected claims: %+v", claims)
			}
			if claims.RegisteredClaims.ID == "" || claims.ExpiresAt == nil || claims.ExpiresAt.Unix() != now.Add(tc.validity).Unix() {
				t.Fatalf("unexpected ID or expiration: %+v", claims.RegisteredClaims)
			}
		})
	}

	nextAccess, nextRefresh, err := j.GenerateTokens("alice", userID)
	if err != nil {
		t.Fatal(err)
	}
	if nextAccess == access || nextRefresh == refresh || access == refresh {
		t.Fatal("token replacements must be distinct, including within the same second")
	}
}

func TestTokenValidationRejectsInvalidClaimsAndSignatures(t *testing.T) {
	now := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	secret := []byte("test-secret")
	j := NewJWT(fixedTimer{now}, secret, time.Minute, time.Hour)
	for _, purpose := range []string{accessPurpose, refreshPurpose} {
		t.Run(purpose, func(t *testing.T) {
			validate := j.ValidateAccessToken
			if purpose == refreshPurpose {
				validate = j.ValidateRefreshToken
			}
			for _, tc := range []struct {
				name    string
				mutate  func(*SignedToken)
				method  jwtgo.SigningMethod
				key     any
				expired bool
			}{
				{name: "legacy token without purpose", mutate: func(c *SignedToken) { c.Purpose = "" }},
				{name: "unknown purpose", mutate: func(c *SignedToken) { c.Purpose = "other" }},
				{name: "missing expiration", mutate: func(c *SignedToken) { c.ExpiresAt = nil }},
				{name: "expired", mutate: func(c *SignedToken) { c.ExpiresAt = jwtgo.NewNumericDate(now.Add(-time.Second)) }, expired: true},
				{name: "expiration boundary", mutate: func(c *SignedToken) { c.ExpiresAt = jwtgo.NewNumericDate(now) }, expired: true},
				{name: "future not before", mutate: func(c *SignedToken) { c.NotBefore = jwtgo.NewNumericDate(now.Add(time.Second)) }},
				{name: "future issued at", mutate: func(c *SignedToken) { c.IssuedAt = jwtgo.NewNumericDate(now.Add(time.Second)) }},
				{name: "missing user ID", mutate: func(c *SignedToken) { c.ID = "" }},
				{name: "malformed user ID", mutate: func(c *SignedToken) { c.ID = "invalid" }},
				{name: "zero user ID", mutate: func(c *SignedToken) { c.ID = id.ID{}.Hex() }},
				{name: "wrong key", key: []byte("wrong-secret")},
				{name: "forged expired token", key: []byte("wrong-secret"), mutate: func(c *SignedToken) { c.ExpiresAt = jwtgo.NewNumericDate(now.Add(-time.Second)) }},
				{name: "different HMAC algorithm", method: jwtgo.SigningMethodHS512},
				{name: "unsigned token", method: jwtgo.SigningMethodNone, key: jwtgo.UnsafeAllowNoneSignatureType},
			} {
				t.Run(tc.name, func(t *testing.T) {
					claims := SignedToken{UserName: "alice", ID: id.NewID().Hex(), Purpose: purpose,
						RegisteredClaims: jwtgo.RegisteredClaims{ExpiresAt: jwtgo.NewNumericDate(now.Add(time.Minute))}}
					if tc.mutate != nil {
						tc.mutate(&claims)
					}
					method := tc.method
					if method == nil {
						method = jwtgo.SigningMethodHS256
					}
					key := tc.key
					if key == nil {
						key = secret
					}
					token, err := jwtgo.NewWithClaims(method, claims).SignedString(key)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = validate(token); err == nil {
						t.Fatal("expected token rejection")
					}
					if errors.Is(err, ErrTokenExpired) != tc.expired {
						t.Fatalf("unexpected expiration classification: %v", err)
					}
				})
			}
			if _, err := validate("not.a.jwt"); err == nil {
				t.Fatal("expected malformed token rejection")
			}
		})
	}
}

func TestExistingTokenFormatRemainsValid(t *testing.T) {
	now := time.Date(2024, time.January, 1, 12, 0, 0, 0, time.UTC)
	j := NewJWT(fixedTimer{now}, []byte("test-secret"), time.Minute, time.Hour)
	for _, purpose := range []string{accessPurpose, refreshPurpose} {
		t.Run(purpose, func(t *testing.T) {
			// This is the JSON shape produced by v3 StandardClaims. In particular,
			// the application user ID and the JWT ID are distinct, case-sensitive keys.
			const userID = "507f1f77bcf86cd799439011"
			oldClaims := jwtgo.MapClaims{
				"UserName": "alice", "ID": userID, "token_use": purpose,
				"exp": now.Add(time.Minute).Unix(), "jti": "existing-session-id",
			}
			signed, err := jwtgo.NewWithClaims(jwtgo.SigningMethodHS256, oldClaims).SignedString([]byte("test-secret"))
			if err != nil {
				t.Fatal(err)
			}
			claims, err := j.validateToken(signed, purpose)
			if err != nil {
				t.Fatal(err)
			}
			if claims.ID != userID || claims.RegisteredClaims.ID != "existing-session-id" {
				t.Fatal("user ID and JWT ID must remain distinct")
			}
			encoded, err := json.Marshal(claims)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["ID"] != userID || wire["jti"] != "existing-session-id" || wire["UserName"] != "alice" || wire["exp"] != float64(now.Add(time.Minute).Unix()) {
				t.Fatalf("token wire format changed: %s", encoded)
			}
		})
	}
}
