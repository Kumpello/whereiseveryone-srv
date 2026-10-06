package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"whereiseveryone/internal/config"
	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi"
	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/crypto"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
	"whereiseveryone/pkg/timer"

	"github.com/labstack/echo/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	// This is a valid cost-14 bcrypt hash used only for dummy verification.
	// Matching it never authenticates a nonexistent account.
	//nolint:gosec // A public dummy hash cannot authenticate any account.
	defaultDummyPasswordHash = "$2a$14$G8Qact67wFEE9JCjAMkKHOKnJSMPnwTdFeFbVdf4.B198SVoWY5im"
)

type mux struct {
	userAdapter users.Adapter
	timer       timer.Timer
	jwt         *jwt.JWT

	passwordHashCost  int
	passwordOps       chan struct{}
	dummyPasswordHash string
	verifyPassword    func(string, string) error
	throttle          *authThrottle
}

func NewMux(
	userAdapter users.Adapter,
	timer timer.Timer,
	jwt *jwt.JWT,
) *mux {
	return &mux{
		userAdapter:       userAdapter,
		timer:             timer,
		jwt:               jwt,
		passwordHashCost:  crypto.DefaultPasswordHashCost,
		passwordOps:       make(chan struct{}, config.DefaultMaxConcurrentPasswordRequests),
		dummyPasswordHash: defaultDummyPasswordHash,
		verifyPassword:    crypto.VerifyPassword,
		throttle:          newAuthThrottle(),
	}
}

// SetPasswordWorkLimits configures admission and prepares the dummy hash before serving requests.
// It must not be called while requests are being served.
func (m *mux) SetPasswordWorkLimits(limits config.PasswordWorkLimits) error {
	if err := limits.Validate(); err != nil {
		return fmt.Errorf("configure password work: %w", err)
	}
	if err := m.SetPasswordHashCost(limits.HashCost); err != nil {
		return err
	}
	m.passwordOps = make(chan struct{}, limits.MaxRequests)
	return nil
}

// SetPasswordHashCost prepares a matching dummy hash before serving requests.
func (m *mux) SetPasswordHashCost(cost int) error {
	if err := (config.PasswordWorkLimits{HashCost: cost, MaxRequests: cap(m.passwordOps)}).Validate(); err != nil {
		return fmt.Errorf("configure password hash cost: %w", err)
	}
	if cost == m.passwordHashCost {
		return nil
	}
	hash, err := crypto.HashPasswordWithCost("dummy-password-never-authenticates", cost)
	if err != nil {
		return fmt.Errorf("prepare dummy password hash: %w", err)
	}
	m.passwordHashCost = cost
	m.dummyPasswordHash = hash
	return nil
}

func (m *mux) acquirePasswordSlot(ctx context.Context) (func(), bool) {
	if ctx.Err() != nil {
		return nil, false
	}
	select {
	case m.passwordOps <- struct{}{}:
		return func() {
			<-m.passwordOps
		}, true
	default:
		return nil, false
	}
}

// verifyLoginPassword never skips bcrypt for an unknown or malformed account.
// Lower-cost legacy hashes are padded to the configured bcrypt work factor.
func (m *mux) verifyLoginPassword(hash, password string, found bool) bool {
	cost, hashErr := bcrypt.Cost([]byte(hash))
	if !found || hashErr != nil {
		hash, cost, found = m.dummyPasswordHash, m.passwordHashCost, false
	}
	passwordErr := m.verifyPassword(hash, password)
	if passwordErr != nil && !errors.Is(passwordErr, bcrypt.ErrMismatchedHashAndPassword) {
		// A valid cost header may still contain a malformed salt or digest.
		passwordErr = m.verifyPassword(m.dummyPasswordHash, password)
		cost, found = m.passwordHashCost, false
	}
	// Bcrypt cost c performs 2^c rounds. Adding dummy work at c, c+1, ...,
	// target-1 raises a lower-cost verification to the same total round count.
	for paddingCost := cost; paddingCost < m.passwordHashCost; paddingCost++ {
		dummy := m.dummyPasswordHash[:4] + fmt.Sprintf("%02d", paddingCost) + m.dummyPasswordHash[6:]
		// These valid encodings need not match a password: their digest is never
		// used to authenticate an account, only to perform the padding work.
		if err := m.verifyPassword(dummy, password); err != nil && !errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			found = false
		}
	}
	return found && passwordErr == nil
}

func (m *mux) Route(g *echo.Group, _ echo.MiddlewareFunc) {
	g.POST("/signup", m.signUp, m.throttleSource(true), webapi.RequireJSON)
	g.POST("/login", m.logIn, m.throttleSource(false), webapi.RequireJSON)
	g.POST("/refresh", m.refreshToken, webapi.RequireJSON)
}

func (m *mux) handleDeviceTokenConflict(ctx context.Context, user users.User, incomingDeviceToken string) (bool, error) {
	// Requests have already supplied a nonblank device token. Only password login
	// may bind a previously unbound account; refresh rejects unbound sessions.
	if strings.TrimSpace(user.Auth.DeviceToken) == "" || user.Auth.DeviceToken == incomingDeviceToken {
		return false, nil
	}

	clearDeviceToken := ""
	token, refresh, err := m.jwt.GenerateTokens(user.Auth.Username, user.ID)
	if err != nil {
		return false, err
	}

	err = m.userAdapter.ReplaceTokens(ctx, user.ID, user.Auth, token, refresh, clearDeviceToken)
	if err != nil {
		return false, err
	}

	return true, nil
}

// signUp
//
// @summary sign up as a new user
// @description creates a new user
// @tags auth
// @accept json
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @produces json
// @param userDetails body signUpRequest true "sign up details"
// @success 200 {object} authResponse
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 429 {object} jsonerr.JSONError "too many requests; see Retry-After"
// @failure 409 {object} jsonerr.JSONError "conflict (user with such a name exists)
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /auth/signup [POST]
func (m *mux) signUp(c *echo.Context) error {
	reqCtx, cancel := webapi.DatabaseRequestContext(c.Request().Context())
	defer cancel()

	var request signUpRequest
	if err := c.Bind(&request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}
	if err := c.Validate(request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}
	if len(request.Password) > 72 {
		err := jsonerr.EchoInvalidRequestError(errors.New("password must not exceed 72 bytes"))
		return err.Echo(c) //nolint:wrapcheck // Echo writes the HTTP response; no extra error context is needed.
	}
	if strings.TrimSpace(request.DeviceToken) == "" {
		return jsonerr.EchoInvalidRequestError(errors.New("device_token must not be blank")).Echo(c)
	}

	q := newQuota(signupAccount, request.Username, signupAccountLimit, time.Hour)
	if retry := m.throttle.allow(m.timer.Now(), q); retry > 0 {
		return tooManyRequests(c, retry)
	}
	releasePasswordSlot, admitted := m.acquirePasswordSlot(reqCtx)
	if !admitted {
		return tooManyRequests(c, time.Second)
	}
	defer releasePasswordSlot()
	encPass, err := crypto.HashPasswordWithCost(request.Password, m.passwordHashCost)
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
		SubscribedUsers: []id.ID{},
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

	if err := m.userAdapter.UpdateTokens(reqCtx, u.ID, &token, &refresh, &request.DeviceToken); err != nil {
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
// @description invalid usernames and passwords return the same 403 response after bcrypt verification
// @tags auth
// @accept json
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @produces json
// @param userDetails body logInRequest true "login details"
// @success 200 {object} authResponse
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 403 {object} jsonerr.JSONError "forbidden (invalid credentials)"
// @failure 429 {object} jsonerr.JSONError "too many requests; see Retry-After"
// @failure 409 {object} map[string]string "device token conflict; session revoked, log in again"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /auth/login [POST]
func (m *mux) logIn(c *echo.Context) error {
	reqCtx, cancel := webapi.DatabaseRequestContext(c.Request().Context())
	defer cancel()

	var request logInRequest
	if err := c.Bind(&request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}
	if err := c.Validate(request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}
	if len(request.Password) > 72 {
		err := jsonerr.EchoInvalidRequestError(errors.New("password must not exceed 72 bytes"))
		return err.Echo(c) //nolint:wrapcheck // Echo writes the HTTP response; no extra error context is needed.
	}
	if strings.TrimSpace(request.DeviceToken) == "" {
		return jsonerr.EchoInvalidRequestError(errors.New("device_token must not be blank")).Echo(c)
	}

	q := newQuota(loginAccount, request.Username, loginAccountLimit, 15*time.Minute)
	if retry := m.throttle.allow(m.timer.Now(), q); retry > 0 {
		return tooManyRequests(c, retry)
	}
	releasePasswordSlot, admitted := m.acquirePasswordSlot(reqCtx)
	if !admitted {
		return tooManyRequests(c, time.Second)
	}
	defer releasePasswordSlot()

	u, err := m.userAdapter.GetUserByUsername(reqCtx, request.Username)
	found := err == nil
	if err != nil && !errors.Is(err, users.ErrUserNotExists) {
		return jsonerr.EchoInternalError(err).Echo(c)
	}
	if !m.verifyLoginPassword(u.Auth.Password, request.Password, found) {
		return jsonerr.EchoForbiddenError().Echo(c)
	}

	token, refresh, err := m.jwt.GenerateTokens(u.Auth.Username, u.ID)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	if conflicted, err := m.handleDeviceTokenConflict(reqCtx, u, request.DeviceToken); err != nil {
		if errors.Is(err, users.ErrSessionChanged) {
			return jsonerr.EchoForbiddenError().Echo(c)
		}
		return jsonerr.EchoInternalError(err).Echo(c)
	} else if conflicted {
		return c.JSON(http.StatusConflict, map[string]string{"message": "device token conflict"})
	}

	if err := m.userAdapter.UpdateTokens(reqCtx, u.ID, &token, &refresh, &request.DeviceToken); err != nil {
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
// @description atomically consumes the current refresh-purpose token for the bound device; refresh reuse is rejected immediately; the previous access token remains valid for up to 120 seconds, subject to its expiration
// @tags auth
// @accept json
// @failure 413 {object} jsonerr.JSONError "request body exceeds 16 KiB"
// @failure 415 {object} jsonerr.JSONError "content type must be application/json"
// @produces json
// @param refresh body refreshTokenRequest true "refresh token"
// @success 200 {object} authResponse
// @failure 400 {object} jsonerr.JSONError "invalid request"
// @failure 401 {object} jsonerr.JSONError "expired refresh token"
// @failure 403 {object} jsonerr.JSONError "invalid refresh token"
// @failure 404 {object} jsonerr.JSONError "user not exists"
// @failure 409 {object} map[string]string "device token conflict; session revoked, log in again"
// @failure 500 {object} jsonerr.JSONError "internal server error"
// @failure 503 {object} jsonerr.JSONError "server busy; retry after Retry-After seconds"
// @header 503 {string} Retry-After "Minimum delay in seconds before retrying (1)"
// @router /auth/refresh [POST]
func (m *mux) refreshToken(c *echo.Context) error {
	reqCtx, cancel := webapi.DatabaseRequestContext(c.Request().Context())
	defer cancel()

	var request refreshTokenRequest

	if err := c.Bind(&request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}

	if err := c.Validate(request); err != nil {
		return jsonerr.EchoInvalidRequestError(err).Echo(c)
	}
	if len(request.RefreshToken) > jwt.MaxTokenBytes {
		err := jsonerr.EchoInvalidRequestError(jwt.ErrTokenTooLarge)
		return err.Echo(c) //nolint:wrapcheck // Echo writes the HTTP response; no extra error context is needed.
	}
	if strings.TrimSpace(request.DeviceToken) == "" {
		return jsonerr.EchoInvalidRequestError(errors.New("device_token must not be blank")).Echo(c)
	}

	// Only refresh-purpose tokens can renew the stored session.
	v, err := m.jwt.ValidateRefreshToken(request.RefreshToken)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return jsonerr.EchoExpiredTokenError().Echo(c)
		} else {
			return jsonerr.EchoForbiddenError().Echo(c)
		}
	}

	userID, err := id.FromString(v.ID)
	if err != nil {
		return jsonerr.EchoForbiddenError().Echo(c)
	}

	// Find user owning refresh token
	u, err := m.userAdapter.GetUser(
		reqCtx,
		userID,
	)
	if err != nil {
		if errors.Is(err, users.ErrUserNotExists) {
			return jsonerr.EchoForbiddenError().Echo(c)
		}

		return jsonerr.EchoInternalError(err).Echo(c)
	}

	// Validate provided refresh token matches stored refresh token
	if !u.Auth.MatchesRefresh(request.RefreshToken) {
		return jsonerr.EchoForbiddenError().Echo(c)
	}
	// A refresh credential cannot establish a device binding for a legacy or
	// revoked session. Require password login to establish a new session.
	if strings.TrimSpace(u.Auth.DeviceToken) == "" {
		return jsonerr.EchoForbiddenError().Echo(c)
	}

	if conflicted, err := m.handleDeviceTokenConflict(reqCtx, u, request.DeviceToken); err != nil {
		if errors.Is(err, users.ErrSessionChanged) {
			return jsonerr.EchoForbiddenError().Echo(c)
		}
		return jsonerr.EchoInternalError(err).Echo(c)
	} else if conflicted {
		return c.JSON(http.StatusConflict, map[string]string{
			"message": "device token conflict",
		})
	}

	// Rotate tokens
	token, refresh, err := m.jwt.GenerateTokens(
		u.Auth.Username,
		u.ID,
	)
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	err = m.userAdapter.ReplaceTokens(
		reqCtx,
		u.ID,
		u.Auth,
		token,
		refresh,
		u.Auth.DeviceToken,
	)
	if errors.Is(err, users.ErrSessionChanged) {
		return jsonerr.EchoForbiddenError().Echo(c)
	}
	if err != nil {
		return jsonerr.EchoInternalError(err).Echo(c)
	}

	return c.JSON(200, authResponse{
		ID:           u.ID.Hex(),
		Token:        token,
		RefreshToken: refresh,
	})
}
