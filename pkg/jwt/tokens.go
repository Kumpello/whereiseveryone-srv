package jwt

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/timer"

	jwtgo "github.com/golang-jwt/jwt/v5"
)

type JWT struct {
	timer           timer.Timer
	secret          []byte
	validity        time.Duration
	refreshValidity time.Duration
}

func NewJWT(timer timer.Timer, secret []byte, validity time.Duration, refreshValidity time.Duration) *JWT {
	return &JWT{timer: timer, secret: secret, validity: validity, refreshValidity: refreshValidity}
}

// Now provides the same clock for token expiration and session grace checks.
func (j JWT) Now() time.Time { return j.timer.Now() }

type SignedToken struct {
	UserName string
	ID       string
	Purpose  string `json:"token_use"`

	jwtgo.RegisteredClaims
}

var ErrTokenExpired = errors.New("token is expired")
var ErrInvalidTokenPurpose = errors.New("invalid token purpose")

const (
	accessPurpose  = "access"
	refreshPurpose = "refresh"
)

func (j JWT) GenerateTokens(username string, id id.ID) (string, string, error) {
	now := j.timer.Now()
	token, err := j.generateToken(username, id, accessPurpose, now.Add(j.validity))
	if err != nil {
		return "", "", fmt.Errorf("create token: %w", err)
	}
	refreshToken, err := j.generateToken(username, id, refreshPurpose, now.Add(j.refreshValidity))
	if err != nil {
		return "", "", fmt.Errorf("create refresh token: %w", err)
	}

	return token, refreshToken, nil
}

func (j JWT) generateToken(username string, userID id.ID, purpose string, expiresAt time.Time) (string, error) {
	// Make replacement tokens distinct even when issued within the same second.
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generate token ID: %w", err)
	}
	claims := SignedToken{
		UserName: username,
		ID:       userID.Hex(),
		Purpose:  purpose,
		RegisteredClaims: jwtgo.RegisteredClaims{
			ID:        base64.RawURLEncoding.EncodeToString(nonce[:]),
			ExpiresAt: jwtgo.NewNumericDate(expiresAt),
		},
	}
	return jwtgo.NewWithClaims(jwtgo.SigningMethodHS256, claims).SignedString(j.secret)
}

func (j JWT) ValidateAccessToken(signed string) (SignedToken, error) {
	return j.validateToken(signed, accessPurpose)
}

func (j JWT) ValidateRefreshToken(signed string) (SignedToken, error) {
	return j.validateToken(signed, refreshPurpose)
}

func (j JWT) validateToken(signed, purpose string) (SignedToken, error) {
	parser := jwtgo.NewParser(
		jwtgo.WithValidMethods([]string{jwtgo.SigningMethodHS256.Alg()}),
		// Validate times below using the injected clock, after verifying the signature.
		jwtgo.WithoutClaimsValidation(),
	)
	token, err := parser.ParseWithClaims(
		signed,
		&SignedToken{},
		func(_ *jwtgo.Token) (any, error) {
			return j.secret, nil
		})

	if err != nil {
		return SignedToken{}, fmt.Errorf("parse token: %w", err)
	}

	claims, ok := token.Claims.(*SignedToken)
	if !ok || !token.Valid {
		return SignedToken{}, errors.New("invalid token")
	}
	if claims.Purpose != purpose {
		return SignedToken{}, ErrInvalidTokenPurpose
	}
	if claims.ExpiresAt == nil || claims.ExpiresAt.Unix() == 0 {
		return SignedToken{}, errors.New("missing token expiration")
	}
	now := j.timer.Now()
	if !now.Before(claims.ExpiresAt.Time) {
		return SignedToken{}, ErrTokenExpired
	}
	if (claims.IssuedAt != nil && now.Before(claims.IssuedAt.Time)) ||
		(claims.NotBefore != nil && now.Before(claims.NotBefore.Time)) {
		return SignedToken{}, errors.New("token is not yet valid")
	}
	userID, err := id.FromString(claims.ID)
	if err != nil || userID == id.ZeroID {
		return SignedToken{}, errors.New("invalid token user ID")
	}

	return *claims, nil
}
