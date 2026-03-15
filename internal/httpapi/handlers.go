package httpapi

import (
	"net/http"

	"github.com/soulteary/gorge-worker/internal/worker"

	"github.com/labstack/echo/v4"
)

type Deps struct {
	Consumer *worker.Consumer
	Token    string
}

type apiResponse struct {
	Data  any       `json:"data,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func RegisterRoutes(e *echo.Echo, deps *Deps) {
	e.GET("/", healthPing())
	e.GET("/healthz", healthPing())

	g := e.Group("/api/worker")
	g.Use(tokenAuth(deps))

	g.GET("/stats", stats(deps))
}

func tokenAuth(deps *Deps) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if deps.Token == "" {
				return next(c)
			}
			token := c.Request().Header.Get("X-Service-Token")
			if token == "" {
				token = c.QueryParam("token")
			}
			if token == "" || token != deps.Token {
				return c.JSON(http.StatusUnauthorized, &apiResponse{
					Error: &apiError{Code: "ERR_UNAUTHORIZED", Message: "missing or invalid service token"},
				})
			}
			return next(c)
		}
	}
}

func healthPing() echo.HandlerFunc {
	return func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	}
}

func stats(deps *Deps) echo.HandlerFunc {
	return func(c echo.Context) error {
		s := deps.Consumer.Stats()
		return c.JSON(http.StatusOK, &apiResponse{Data: s})
	}
}
