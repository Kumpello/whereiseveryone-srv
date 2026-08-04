package logger

import (
	"time"

	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
)

type Logger logrus.FieldLogger

func NewLogger() *logrus.Logger {
	l := logrus.New()
	return l
}

func MakeEchoLogEntry(logger Logger, c echo.Context) *logrus.Entry {
	if c == nil {
		return logger.WithFields(logrus.Fields{
			"at": time.Now().Format("2006-01-02 15:04:05"),
		})
	}

	return logger.WithFields(logrus.Fields{
		"at":     time.Now().Format("2006-01-02 15:04:05"),
		"method": c.Request().Method,
		"path":   c.Path(),
		"uri":    c.Request().RequestURI,
		"ip":     c.RealIP(),
	})
}
