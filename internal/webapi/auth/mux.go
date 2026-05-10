package auth

import (
	"context"
	"errors"
	"time"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/crypto"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
	"whereiseveryone/pkg/timer"

	"github.com/labstack/echo/v4"
)

type mux struct {
	userAdapter users.Adapter
	timer       timer.Timer
	jwt         *jwt.JWT
}

func NewMux(
	userAdapter users.Adapter,
	timer timer.Timer,
	jwt *jwt.JWT,
) *mux {
	return &mux{userAdapter, timer, jwt}
}

func (m *mux) Route(g *echo.Group, _ echo.MiddlewareFunc) {
	g.POST("/signup", m.signUp)
	g.POST("/login", m.logIn)
	g.POST("/refresh", m.refreshToken)
}

// signUp
//
// @summary sign up as a new user
// @description creates a new user
// @tags auth
// @accept json
// @produces json
// @param userDetails body signUpRequest true "sign up details"
// @success 200 {object} authResponse
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 409 {object} jsonerr.JSONError "conflict (user with such a name exists)
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @router /auth/signup [POST]
func (m *mux) signUp(c echo.Context) error {
	reqCtx, cancel := context.WithTimeout(c.Request().Context(), time.Duration(60)*time.Second)
	defer cancel()

	var request signUpRequest
	if err := c.Bind(&request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}
	if err := c.Validate(request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}

	encPass, err := crypto.HashPassword(request.Password)
	if err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}

	u := users.User{
		ID: id.ID{}, // stub
		Auth: users.Auth{
			Username:     request.Username,
			Password:     encPass,
			Token:        "",
			RefreshToken: "",
			CreatedAt:    m.timer.Now(),
			UpdatedAt:    m.timer.Now(),
		},
		SubscribedUsers:               []id.ID{},
		PendingIncomingFriendRequests: []id.ID{},
		PendingOutgoingFriendRequests: []id.ID{},
	}

	if u, err = m.userAdapter.NewUser(reqCtx, u); err != nil { // overwrite user for ID and generated data
		if errors.Is(err, users.ErrUserNameAlreadyExists) {
			return jsonerr.EchoConflictError(err).Echo(c)
		}

		return jsonerr.EchoInternalError(err).Echo(c)
	}

	token, refresh, err := m.jwt.GenerateTokens(u.Auth.Username, u.ID)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	if err := m.userAdapter.UpdateTokens(reqCtx, u.ID, &token, &refresh); err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.JSON(200, authResponse{
		ID:           u.ID.Hex(),
		Token:        token,
		RefreshToken: refresh,
	})
}

// logIn
//
// @summary log in
// @description logs in as an exiting users using login and passowrd
// @tags auth
// @accept json
// @produces json
// @param userDetails body logInRequest true "login details"
// @success 200 {object} authResponse
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 403 {object} jsonerr.JSONError "forbidden (invalid password)"
// @failure 404 {object} jsonerr.JSONError "user not exists"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @router /auth/login [POST]
func (m *mux) logIn(c echo.Context) error {
	reqCtx, cancel := context.WithTimeout(c.Request().Context(), time.Duration(60)*time.Second)
	defer cancel()

	var request logInRequest
	if err := c.Bind(&request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}
	if err := c.Validate(request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}

	u, err := m.userAdapter.GetUserByUsername(reqCtx, request.Username)
	if err != nil {
		if errors.Is(err, users.ErrUserNotExists) {
			return jsonerr.EchoNotFoundError(err).Echo(c)
		}
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	if err = crypto.VerifyPassword(u.Auth.Password, request.Password); err != nil {
		return jsonerr.EchoForbiddenError().Echo(c)
	}

	token, refresh, err := m.jwt.GenerateTokens(u.Auth.Username, u.ID)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	if err := m.userAdapter.UpdateTokens(reqCtx, u.ID, &token, &refresh); err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.JSON(200, authResponse{
		ID:           u.ID.Hex(),
		Token:        token,
		RefreshToken: refresh,
	})
}

// refreshToken
//
// @summary refresh auth tokens
// @description generates new access and refresh tokens
// @tags auth
// @accept json
// @produces json
// @param refresh body refreshTokenRequest true "refresh token"
// @success 200 {object} authResponse
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 403 {object} jsonerr.JSONError "invalid refresh token"
// @failure 404 {object} jsonerr.JSONError "user not exists"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @router /auth/refresh [POST]
func (m *mux) refreshToken(c echo.Context) error {
	reqCtx, cancel := context.WithTimeout(
		c.Request().Context(),
		60*time.Second,
	)
	defer cancel()

	var request refreshTokenRequest

	if err := c.Bind(&request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}

	if err := c.Validate(request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}

	// Validate JWT structure and expiration
	if err := m.jwt.ValidateRefreshToken(
		request.RefreshToken,
	); err != nil {
		return jsonerr.EchoForbiddenError().Echo(c)
	}

	// Find user owning refresh token
	u, err := m.userAdapter.GetUserByRefreshToken(
		reqCtx,
		request.RefreshToken,
	)
	if err != nil {
		if errors.Is(err, users.ErrUserNotExists) {
			return jsonerr.EchoForbiddenError().Echo(c)
		}

		return jsonerr.EchoInternalError(err).Echo(c)
	}

	// Rotate tokens
	token, refresh, err := m.jwt.GenerateTokens(
		u.Auth.Username,
		u.ID,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	err = m.userAdapter.UpdateTokens(
		reqCtx,
		u.ID,
		&token,
		&refresh,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.JSON(200, authResponse{
		ID:           u.ID.Hex(),
		Token:        token,
		RefreshToken: refresh,
	})
}
