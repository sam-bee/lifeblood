package app

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

func requestLogger(log *slog.Logger) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		HandleError: true, LogLatency: true, LogMethod: true, LogRoutePath: true,
		LogStatus: true, LogResponseSize: true,
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			if isProbe(c) && v.Status < http.StatusBadRequest {
				return nil
			}
			// Only log the route template, never raw paths, query strings, headers,
			// bodies or errors: these can contain credentials and health data.
			route := v.RoutePath
			if route == "" {
				route = "<unmatched>"
			}
			level := slog.LevelInfo
			if v.Status >= http.StatusInternalServerError {
				level = slog.LevelError
			} else if v.Status >= http.StatusBadRequest {
				level = slog.LevelWarn
			}
			log.Log(c.Request().Context(), level, "request completed",
				"method", v.Method, "route", route, "status", v.Status,
				"duration_ms", float64(v.Latency)/1e6, "response_bytes", v.ResponseSize)
			return nil
		},
	})
}
