package webapi

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4/middleware"

	"whereiseveryone/internal/webapi/jsonerr"
	"whereiseveryone/pkg/jwt"
	"whereiseveryone/pkg/logger"

	"github.com/go-playground/validator"
	"github.com/labstack/echo/v4"
)

type Router interface {
	Route(g *echo.Group, authMiddleware echo.MiddlewareFunc)
}

type echoValidator struct {
	validator *validator.Validate
}

func (v *echoValidator) Validate(i any) error {
	return v.validator.Struct(i) //nolint:wrapcheck  // that's ok (echo framework)
}

func GetJWTToken(c echo.Context) (jwt.SignedToken, error) {
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

func NewEcho(
	basePath string,
	validate *validator.Validate,
	jwtInstance *jwt.JWT,
	routers EchoRouters,
	log logger.Logger,
	debug bool,
) *echo.Echo {
	e := echo.New()
	e.Debug = debug
	e.Validator = &echoValidator{validator: validate}

	authMiddleware := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			jwtToken := c.Request().Header.Get("Authorization")
			if jwtToken == "" {
				return c.String(403, "missing jwt token")
			}

			if !strings.HasPrefix(jwtToken, "Bearer ") {
				return c.String(400, "token must start with bearer")
			}

			v, err := jwtInstance.ValidateToken(strings.TrimPrefix(jwtToken, "Bearer "))
			if err != nil {
				status := JWTErrorToStatus(err)
				if status == http.StatusUnauthorized {
					return c.String(status, "token expired")
				}

				return c.String(status, fmt.Sprintf("invalid token: %s", err.Error()))
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

	e.GET("health", func(c echo.Context) error {
		return c.JSON(200, "ok")
	})

	// TODO: Use logrus for logging instead of this middleware
	// 		 Config to log this only for debug
	//		 And disable it on production
	e.Use(middleware.Logger())
	e.Use(middleware.BodyDump(func(c echo.Context, reqBody, resBody []byte) {
	}))

	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			logger.MakeEchoLogEntry(log, c).Info("incoming request")
			return next(c)
		}
	})

	return e
}
