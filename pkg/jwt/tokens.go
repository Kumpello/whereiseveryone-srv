package jwt

import (
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

	jwtgo.StandardClaims
}

var ErrTokenExpired = errors.New("token is expired")

func (j JWT) GenerateTokens(username string, id id.ID) (string, string, error) {
	claims := SignedToken{
		UserName: username,
		ID:       id.Hex(),
		StandardClaims: jwtgo.StandardClaims{
			ExpiresAt: j.timer.Now().Add(j.validity).Unix(),
		},
	}

	refresh := SignedToken{
		UserName: username,
		ID:       id.Hex(),
		StandardClaims: jwtgo.StandardClaims{
			ExpiresAt: j.timer.Now().Add(j.refreshValidity).Unix(),
		},
	}

	token, err := jwtgo.NewWithClaims(jwtgo.SigningMethodHS256, claims).SignedString(j.secret)
	if err != nil {
		return "", "", fmt.Errorf("create token: %w", err)
	}
	refreshToken, err := jwtgo.NewWithClaims(jwtgo.SigningMethodHS256, refresh).SignedString(j.secret)
	if err != nil {
		return "", "", fmt.Errorf("create refresh token: %w", err)
	}

	return token, refreshToken, nil
}

func (j JWT) ValidateToken(signed string) (SignedToken, error) {
	token, err := jwtgo.ParseWithClaims(
		signed,
		&SignedToken{},
		func(token *jwtgo.Token) (any, error) {
			return j.secret, nil
		})

	if err != nil {
		var validationErr *jwtgo.ValidationError
		if errors.As(err, &validationErr) && validationErr.Errors&jwtgo.ValidationErrorExpired != 0 {
			return SignedToken{}, ErrTokenExpired
		}
		return SignedToken{}, fmt.Errorf("parse token: %w", err)
	}

	claims, ok := token.Claims.(*SignedToken)
	if !ok {
		return SignedToken{}, fmt.Errorf("invalid token: %w", err)
	}

	if claims.ExpiresAt < j.timer.Now().Unix() {
		return SignedToken{}, ErrTokenExpired
	}

	return *claims, nil
}
