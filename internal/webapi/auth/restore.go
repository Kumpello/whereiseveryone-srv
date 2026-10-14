package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
	"whereiseveryone/internal/restore"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/id"
)

// restoreRegistrationOptions
//
// @summary create Android restore-key registration options
// @description Create account-bound options to enroll an Android restore key.
// @tags auth
// @security Bearer
// @accept json
// @produce json
// @param request body restoreOptionsRequest true "application and source device"
// @success 200 {object} restore.Options
// @failure 400,401,403,413,415,429,500,503 {object} jsonerr.JSONError
// @router /auth/restore/register/options [POST]
//
//nolint:wrapcheck // Echo owns HTTP response-write errors.
func (m *mux) restoreRegistrationOptions(c *echo.Context) error {
	var request restoreOptionsRequest
	if err := readRestoreRequest(c, &request, &request); err != nil {
		return err.Echo(c)
	}
	userID, err := restoreUserID(c)
	if err != nil {
		return jsonerr.EchoForbiddenError().Echo(c)
	}
	ctx, cancel := webapi.DatabaseRequestContext(c.Request().Context())
	defer cancel()
	user, err := m.userAdapter.GetUser(ctx, userID)
	if err != nil {
		return restoreError(c, err)
	}
	if user.Auth.DeviceToken != request.DeviceToken {
		return jsonerr.EchoForbiddenError().Echo(c)
	}
	options, err := m.restore.BeginRegistration(ctx, user.ID, user.Auth.Username, request.AppID, request.DeviceToken)
	if err != nil {
		return restoreError(c, err)
	}
	return c.JSON(http.StatusOK, options)
}

// finishRestoreRegistration
//
// @summary verify and save a system-managed Android restore key
// @description Validate and persist an account-owned Android restore key.
// @tags auth
// @security Bearer
// @accept json
// @param request body restoreFinishRequest true "registration response"
// @success 204 "registered"
// @failure 400,401,403,413,415,429,500,503 {object} jsonerr.JSONError
// @router /auth/restore/register/finish [POST]
//
//nolint:wrapcheck // Echo owns HTTP response-write errors.
func (m *mux) finishRestoreRegistration(c *echo.Context) error {
	var request restoreFinishRequest
	if err := readRestoreRequest(c, &request, &request.restoreOptionsRequest); err != nil {
		return err.Echo(c)
	}
	userID, err := restoreUserID(c)
	if err != nil {
		return jsonerr.EchoForbiddenError().Echo(c)
	}
	ctx, cancel := webapi.DatabaseRequestContext(c.Request().Context())
	defer cancel()
	session, err := m.userAdapter.GetSession(ctx, userID)
	if err != nil {
		return restoreError(c, err)
	}
	if session.DeviceToken != request.DeviceToken {
		return jsonerr.EchoForbiddenError().Echo(c)
	}
	if err := m.restore.FinishRegistration(ctx, userID, request.AppID, request.DeviceToken,
		request.ChallengeID, request.CredentialJSON); err != nil {
		return restoreError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

// restoreAuthenticationOptions
//
// @summary create Android restore-key authentication options
// @description Issue a five-minute one-use challenge for restore sign-in.
// @tags auth
// @accept json
// @produce json
// @param request body restoreOptionsRequest true "application and destination device"
// @success 200 {object} restore.Options
// @failure 400,403,413,415,429,500,503 {object} jsonerr.JSONError
// @router /auth/restore/login/options [POST]
//
//nolint:wrapcheck // Echo owns HTTP response-write errors.
func (m *mux) restoreAuthenticationOptions(c *echo.Context) error {
	var request restoreOptionsRequest
	if err := readRestoreRequest(c, &request, &request); err != nil {
		return err.Echo(c)
	}
	ctx, cancel := webapi.DatabaseRequestContext(c.Request().Context())
	defer cancel()
	options, err := m.restore.BeginAuthentication(ctx, request.AppID, request.DeviceToken)
	if err != nil {
		return restoreError(c, err)
	}
	return c.JSON(http.StatusOK, options)
}

// finishRestoreAuthentication
//
// @summary sign in with a transferred Android restore key
// @description Verify a silent restore assertion and move the session to the new device.
// @tags auth
// @accept json
// @produce json
// @param request body restoreFinishRequest true "authentication assertion"
// @success 200 {object} restoreAuthResponse
// @failure 400,403,413,415,429,500,503 {object} jsonerr.JSONError
// @router /auth/restore/login/finish [POST]
//
//nolint:wrapcheck // Echo owns HTTP response-write errors.
func (m *mux) finishRestoreAuthentication(c *echo.Context) error {
	var request restoreFinishRequest
	if err := readRestoreRequest(c, &request, &request.restoreOptionsRequest); err != nil {
		return err.Echo(c)
	}
	ctx, cancel := webapi.DatabaseRequestContext(c.Request().Context())
	defer cancel()
	userID, err := m.restore.FinishAuthentication(ctx, request.AppID, request.DeviceToken,
		request.ChallengeID, request.CredentialJSON)
	if err != nil {
		return restoreError(c, err)
	}
	user, err := m.userAdapter.GetUser(ctx, userID)
	if err != nil {
		return restoreError(c, err)
	}
	token, refresh, err := m.jwt.GenerateTokens(user.Auth.Username, user.ID)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}
	if err := m.userAdapter.ReplaceTokens(ctx, user.ID, user.Auth, token, refresh, request.DeviceToken); err != nil {
		return restoreError(c, err)
	}
	return c.JSON(http.StatusOK, restoreAuthResponse{
		authResponse: authResponse{ID: user.ID.Hex(), Token: token, RefreshToken: refresh}, Username: user.Auth.Username,
	})
}

func readRestoreRequest(c *echo.Context, request any, identity *restoreOptionsRequest) *jsonerr.JSONError {
	if err := c.Bind(request); err != nil {
		return jsonerr.EchoInvalidRequestError(err)
	}
	if err := c.Validate(request); err != nil {
		return jsonerr.EchoInvalidRequestError(err)
	}
	if strings.TrimSpace(identity.DeviceToken) == "" || strings.TrimSpace(identity.AppID) != identity.AppID {
		return jsonerr.EchoInvalidRequestError(errors.New("invalid restore application or device"))
	}
	return nil
}

func restoreUserID(c *echo.Context) (id.ID, error) {
	claims, err := webapi.GetJWTToken(c)
	if err != nil {
		return id.ZeroID, fmt.Errorf("read authenticated restore account: %w", err)
	}
	userID, err := id.FromString(claims.ID)
	if err != nil {
		return id.ZeroID, fmt.Errorf("parse authenticated restore account: %w", err)
	}
	return userID, nil
}

//nolint:wrapcheck // Echo owns HTTP response-write errors.
func restoreError(c *echo.Context, err error) error {
	switch {
	case errors.Is(err, restore.ErrDisabled):
		return jsonerr.EchoError(http.StatusServiceUnavailable, "restore unavailable", nil).Echo(c)
	case errors.Is(err, restore.ErrRejected), errors.Is(err, users.ErrUserNotExists),
		errors.Is(err, users.ErrSessionChanged):
		return jsonerr.EchoForbiddenError().Echo(c)
	default:
		return jsonerr.EchoInternalError(err).Echo(c)
	}
}

type restoreOptionsRequest struct {
	AppID       string `json:"app_id" validate:"required,max=256"`
	DeviceToken string `json:"device_token" validate:"required,max=256"`
}

type restoreFinishRequest struct {
	restoreOptionsRequest
	ChallengeID    string `json:"challenge_id" validate:"required,max=128"`
	CredentialJSON string `json:"credential_json" validate:"required,max=12288"`
}

type restoreAuthResponse struct {
	authResponse
	Username string `json:"username"`
}
