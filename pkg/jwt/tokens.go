package jwt

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/timer"

	jwtgo "github.com/golang-jwt/jwt"
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

type SignedToken struct {
	UserName string
	ID       string
	Purpose  string `json:"token_use"`

	jwtgo.StandardClaims
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
		StandardClaims: jwtgo.StandardClaims{
			Id:        base64.RawURLEncoding.EncodeToString(nonce[:]),
			ExpiresAt: expiresAt.Unix(),
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
	parser := jwtgo.Parser{
		ValidMethods: []string{jwtgo.SigningMethodHS256.Alg()},
		// Validate times below using the injected clock, after verifying the signature.
		SkipClaimsValidation: true,
	}
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
	if claims.ExpiresAt == 0 {
		return SignedToken{}, errors.New("missing token expiration")
	}
	now := j.timer.Now().Unix()
	if claims.ExpiresAt <= now {
		return SignedToken{}, ErrTokenExpired
	}
	if !claims.VerifyIssuedAt(now, false) || !claims.VerifyNotBefore(now, false) {
		return SignedToken{}, errors.New("token is not yet valid")
	}
	userID, err := id.FromString(claims.ID)
	if err != nil || userID == id.ZeroID {
		return SignedToken{}, errors.New("invalid token user ID")
	}

	return *claims, nil
}
