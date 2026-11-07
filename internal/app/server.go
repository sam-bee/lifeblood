package app

import (
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/sam-bee/lifeblood/internal/eufylife"
)

type Server struct {
	Store *Store
	Oura  *Oura
	Eufy  EufyClient
	Log   *slog.Logger
}

type apiError struct {
	status int
	code   string
}

func (e *apiError) Error() string        { return e.code }
func fail(status int, code string) error { return &apiError{status, code} }

func (s *Server) Handler() *echo.Echo {
	if s.Eufy == nil {
		s.Eufy = eufylife.New(s.Store.Config.EufyEmail, s.Store.Config.EufyPassword)
	}
	e := echo.New()
	e.IPExtractor = echo.ExtractIPDirect()
	if len(s.Store.Config.TrustedProxies) > 0 {
		opts := []echo.TrustOption{echo.TrustLoopback(false), echo.TrustLinkLocal(false), echo.TrustPrivateNet(false)}
		for _, cidr := range s.Store.Config.TrustedProxies {
			opts = append(opts, echo.TrustIPRange(cidr))
		}
		e.IPExtractor = echo.ExtractIPFromXFFHeader(opts...)
	}
	e.HTTPErrorHandler = func(c *echo.Context, err error) {
		if response, _ := echo.UnwrapResponse(c.Response()); response != nil && response.Committed {
			return
		}
		status, code := http.StatusInternalServerError, "internal_error"
		var own *apiError
		var httpErr interface{ StatusCode() int }
		if errors.As(err, &own) {
			status, code = own.status, own.code
		} else if errors.As(err, &httpErr) {
			status = httpErr.StatusCode()
			code = http.StatusText(status)
		}
		_ = c.JSON(status, map[string]any{"error": map[string]string{"code": code}})
	}
	// Keep logging outside recovery so panics are recorded with their final status.
	e.Use(requestLogger(s.Log))
	e.Use(middleware.RecoverWithConfig(middleware.RecoverConfig{DisablePrintStack: true, DisableStackAll: true}))
	e.Use(middleware.BodyLimit(64 * 1024))
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			h := c.Response().Header()
			h.Set("Cache-Control", "no-store")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Strict-Transport-Security", "max-age=31536000")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			if !isProbe(c) {
				u, _ := url.Parse(s.Store.Config.PublicURL)
				if c.Request().Host != u.Host {
					return fail(400, "invalid_host")
				}
			}
			return next(c)
		}
	})
	e.Use(limit(2, 30, func(c *echo.Context) (string, error) { return c.RealIP(), nil }, isProbe))
	e.GET("/healthz", func(c *echo.Context) error { return c.JSON(200, map[string]string{"status": "ok"}) })
	e.GET("/readyz", func(c *echo.Context) error {
		var version int
		if err := s.Store.DB.QueryRow(c.Request().Context(), `SELECT version FROM lifeblood_schema`).Scan(&version); err != nil || version != 1 {
			return fail(503, "not_ready")
		}
		return c.JSON(200, map[string]string{"status": "ready"})
	})
	// The only public integration endpoint authenticates Oura signatures itself.
	e.GET("/oura/webhooks", s.verifyWebhook)
	e.POST("/oura/webhooks", s.receiveWebhook)
	e.GET("/oura/oauth/callback", func(c *echo.Context) error {
		if !s.Store.Config.OuraEnabled() {
			return fail(503, "oura_not_configured")
		}
		err := s.Oura.CompleteLink(c.Request().Context(), c.QueryParam("state"), c.QueryParam("code"))
		if err != nil {
			return fail(400, "oura_authorization_failed")
		}
		return c.JSON(200, map[string]string{"status": "connected"})
	})
	api := e.Group("/oura", s.authenticate)
	api.Use(limit(5, 30, func(*echo.Context) (string, error) { return "owner", nil }, nil))
	api.GET("/connection", s.connectionStatus)
	api.GET("/daily", func(c *echo.Context) error { return s.read(c, Collections[:3]) })
	api.GET("/sleep", func(c *echo.Context) error { return s.read(c, []string{"sleep"}) })
	api.GET("/collections/:collection", func(c *echo.Context) error {
		if !collectionAllowed(c.Param("collection")) {
			return fail(404, "collection_unavailable")
		}
		return s.read(c, []string{c.Param("collection")})
	})
	api.POST("/sync", s.sync, limit(1.0/60, 2, func(*echo.Context) (string, error) { return "sync", nil }, nil))
	api.GET("/sync", s.syncStatus)
	eufy := e.Group("/eufy", s.authenticate)
	eufy.Use(limit(5, 30, func(*echo.Context) (string, error) { return "owner", nil }, nil))
	eufy.GET("/connection", func(c *echo.Context) error { return c.JSON(200, s.Eufy.Status()) })
	eufy.GET("/members", s.eufyMembers)
	eufy.GET("/devices", s.eufyDevices)
	eufy.GET("/measurements", s.eufyMeasurements)
	eufy.GET("/latest-weight", s.eufyLatestWeight)
	return e
}

func isProbe(c *echo.Context) bool { return c.Path() == "/healthz" || c.Path() == "/readyz" }

func limit(rate float64, burst int, key middleware.Extractor, skip middleware.Skipper) echo.MiddlewareFunc {
	return middleware.RateLimiterWithConfig(middleware.RateLimiterConfig{
		Skipper: skip, Store: middleware.NewRateLimiterMemoryStoreWithConfig(middleware.RateLimiterMemoryStoreConfig{Rate: rate, Burst: burst, ExpiresIn: 10 * time.Minute}),
		IdentifierExtractor: key, DenyHandler: func(c *echo.Context, _ string, _ error) error {
			c.Response().Header().Set("Retry-After", "60")
			return fail(429, "rate_limited")
		},
	})
}

func (s *Server) authenticate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		parts := strings.Fields(c.Request().Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) < 32 || len(parts[1]) > 512 || !hmac.Equal(digest(parts[1]), s.Store.Config.APITokenHash) {
			c.Response().Header().Set("WWW-Authenticate", `Bearer realm="lifeblood"`)
			return fail(401, "unauthorized")
		}
		return next(c)
	}
}

func (s *Server) connectionStatus(c *echo.Context) error {
	conn, err := s.Store.Connection(c.Request().Context())
	if errors.Is(err, pgx.ErrNoRows) {
		return c.JSON(200, map[string]any{"source": "oura", "status": "not_connected", "configured": s.Store.Config.OuraEnabled()})
	}
	if err != nil {
		return err
	}
	coverage, err := s.Store.Coverage(c.Request().Context(), conn.ID)
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"source": "oura", "status": conn.Status, "reconnect_reason": conn.Reason, "coverage": coverage})
}

func (s *Server) connected(c *echo.Context) (Connection, error) {
	conn, err := s.Store.Connection(c.Request().Context())
	if errors.Is(err, pgx.ErrNoRows) {
		return conn, fail(409, "not_connected")
	}
	if err != nil {
		return conn, err
	}
	return conn, nil // retained data remains readable after a disconnect
}

func parseRange(start, end string, maxDays, retention int) (time.Time, time.Time, error) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	to := today.AddDate(0, 0, 1)
	var err error
	if end != "" {
		to, err = time.Parse(time.DateOnly, end)
		if err != nil {
			return time.Time{}, time.Time{}, fail(400, "invalid_end_date")
		}
	}
	from := to.AddDate(0, 0, -min(30, retention))
	if start != "" {
		from, err = time.Parse(time.DateOnly, start)
		if err != nil {
			return time.Time{}, time.Time{}, fail(400, "invalid_start_date")
		}
	}
	if !to.After(from) || to.Sub(from) > time.Duration(maxDays)*24*time.Hour || to.After(today.AddDate(0, 0, 1)) || from.Before(today.AddDate(0, 0, -retention)) {
		return from, to, fail(400, "invalid_date_range")
	}
	return from, to, nil
}

func (s *Server) read(c *echo.Context, collections []string) error {
	conn, err := s.connected(c)
	if err != nil {
		return err
	}
	start, end, err := parseRange(c.QueryParam("start"), c.QueryParam("end"), 90, s.Store.Config.RetentionDays)
	if err != nil {
		return err
	}
	cursor := ""
	if raw := c.QueryParam("cursor"); raw != "" {
		if len(raw) > 512 {
			return fail(400, "invalid_cursor")
		}
		b, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || len(b) > 256 {
			return fail(400, "invalid_cursor")
		}
		cursor = string(b)
	}
	docs, err := s.Store.Documents(c.Request().Context(), conn.ID, collections, start, end, cursor)
	if err != nil {
		return err
	}
	next := ""
	if len(docs) > 500 {
		docs = docs[:500]
		last := docs[len(docs)-1]
		next = base64.RawURLEncoding.EncodeToString([]byte(last.Collection + ":" + last.ID))
	}
	coverage, err := s.Store.Coverage(c.Request().Context(), conn.ID)
	if err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"source": "oura", "start": start.Format(time.DateOnly), "end_exclusive": end.Format(time.DateOnly),
		"connection_status": conn.Status, "data": docs, "next_cursor": next, "coverage": coverage})
}

func (s *Server) sync(c *echo.Context) error {
	var request struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fail(400, "invalid_json")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fail(400, "invalid_json")
	}
	start, end, err := parseRange(request.Start, request.End, 365, s.Store.Config.RetentionDays)
	if err != nil {
		return err
	}
	conn, err := s.connected(c)
	if err != nil {
		return err
	}
	if conn.Status != "active" {
		return fail(409, "reconnect_required")
	}
	if err = s.Store.EnqueueRange(c.Request().Context(), start, end); err != nil {
		return err
	}
	return c.JSON(202, map[string]string{"status": "queued"})
}

func (s *Server) syncStatus(c *echo.Context) error {
	rows, err := s.Store.DB.Query(c.Request().Context(), `SELECT status,count(*) FROM jobs GROUP BY status`)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := map[string]int64{}
	for rows.Next() {
		var status string
		var count int64
		if err = rows.Scan(&status, &count); err != nil {
			return err
		}
		counts[status] = count
	}
	if err = rows.Err(); err != nil {
		return err
	}
	return c.JSON(200, map[string]any{"jobs": counts})
}

func (s *Server) verifyWebhook(c *echo.Context) error {
	secret := s.Store.Config.WebhookSecret
	if secret == "" || !hmac.Equal(digest(c.QueryParam("verification_token")), digest(secret)) {
		return fail(401, "invalid_webhook")
	}
	challenge := c.QueryParam("challenge")
	if challenge == "" || len(challenge) > 512 {
		return fail(400, "invalid_challenge")
	}
	return c.JSON(200, map[string]string{"challenge": challenge})
}

func (s *Server) receiveWebhook(c *echo.Context) error {
	if !s.Store.Config.OuraEnabled() {
		return fail(503, "oura_not_configured")
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, 64*1024+1))
	if err != nil {
		return fail(400, "invalid_body")
	}
	if len(body) > 64*1024 {
		return fail(413, "body_too_large")
	}
	if !VerifyWebhook(s.Store.Config.OuraClientSecret, c.Request().Header.Get("x-oura-timestamp"), c.Request().Header.Get("x-oura-signature"), body, time.Now()) {
		return fail(401, "invalid_webhook")
	}
	var event Webhook
	if json.Unmarshal(body, &event) != nil || !event.Valid() {
		return fail(400, "invalid_event")
	}
	if err = s.Store.ReceiveWebhook(c.Request().Context(), body, event); err != nil {
		return err
	}
	return c.NoContent(200)
}
