package webapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"whereiseveryone/internal/users"
	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/id"
	"whereiseveryone/pkg/jwt"
	"whereiseveryone/pkg/logger"

	"github.com/go-playground/validator/v10"
	"github.com/labstack/echo/v5"
)

const sessionLookupTimeout = 15 * time.Second

type Router interface {
	Route(g *echo.Group, authMiddleware echo.MiddlewareFunc)
}

type echoValidator struct {
	validator *validator.Validate
}

func (v *echoValidator) Validate(i any) error {
	return v.validator.Struct(i) //nolint:wrapcheck  // that's ok (echo framework)
}

// GetJWTToken returns the authenticated token claims attached to the request.
func GetJWTToken(c *echo.Context) (jwt.SignedToken, error) {
	token := c.Get("user")
	jwtToken, ok := token.(jwt.SignedToken)
	if !ok {
		return jwt.SignedToken{}, errors.New("invalid jwt token")
	}

	return jwtToken, nil
}

func JWTErrorToStatus(err error) int {
	if errors.Is(err, jwt.ErrTokenExpired) {
		return http.StatusUnauthorized
	}

	return http.StatusForbidden
}

func JWTErrorToJSONError(err error) *jsonerr.JSONError {
	if errors.Is(err, jwt.ErrTokenExpired) {
		return jsonerr.EchoExpiredTokenError()
	}

	return jsonerr.EchoForbiddenError()
}

type EchoRouters struct {
	Swagger    echo.HandlerFunc
	AuthRouter Router
	MeRouter   Router
}

// SessionReader provides the current persisted credentials for session revocation checks.
type SessionReader interface {
	GetUser(ctx context.Context, userID id.ID) (users.User, error)
}

func NewEcho(
	basePath string,
	validate *validator.Validate,
	jwtInstance *jwt.JWT,
	sessions SessionReader,
	routers EchoRouters,
	log logger.Logger,
	_ bool, // Debug mode never changes public errors or disables correlated logging.
) *echo.Echo {
	e := echo.New()
	e.HTTPErrorHandler = publicHTTPErrorHandler
	e.Validator = &echoValidator{validator: validate}
	e.Pre(requestCorrelation, requestLogger(log), limitRequestBody)

	authMiddleware := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			jwtToken := c.Request().Header.Get("Authorization")
			if jwtToken == "" {
				return c.String(403, "missing jwt token")
			}

			if !strings.HasPrefix(jwtToken, "Bearer ") {
				return c.String(400, "token must start with bearer")
			}

			signed := strings.TrimPrefix(jwtToken, "Bearer ")
			v, err := jwtInstance.ValidateAccessToken(signed)
			if err != nil {
				status := JWTErrorToStatus(err)
				if status == http.StatusUnauthorized {
					return c.String(status, "token expired")
				}

				return c.String(status, "invalid token")
			}

			userID, err := id.FromString(v.ID)
			if err != nil || userID == id.ZeroID {
				return c.String(http.StatusForbidden, "invalid token")
			}
			ctx, cancel := context.WithTimeout(c.Request().Context(), sessionLookupTimeout)
			user, err := sessions.GetUser(ctx, userID)
			cancel()
			if err != nil {
				if errors.Is(err, users.ErrUserNotExists) {
					return c.String(http.StatusForbidden, "invalid session")
				}
				return jsonerr.EchoInternalError(err).Echo(c)
			}
			// Refresh preserves the previous access token for a bounded grace period.
			if strings.TrimSpace(user.Auth.DeviceToken) == "" ||
				!user.Auth.MatchesAccess(signed, jwtInstance.Now()) {
				return c.String(http.StatusForbidden, "invalid session")
			}
			c.Set("user", v)

			return next(c)
		}
	}

	basePathGroup := e.Group(basePath)

	e.GET("/swagger/*", routers.Swagger)
	authRouter := basePathGroup.Group("/auth")
	meRouter := basePathGroup.Group("/me", authMiddleware)

	routers.AuthRouter.Route(authRouter, authMiddleware)
	routers.MeRouter.Route(meRouter, authMiddleware)

	e.GET("health", func(c *echo.Context) error {
		return c.JSON(200, "ok")
	})

	return e
}
