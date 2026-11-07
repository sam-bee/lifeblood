package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
)

func TestRequestLogsFinalResponse(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, token, body, route, level string
		status                                        int
	}{
		{"success", "GET", "/eufy/connection", testToken, "", "/eufy/connection", "INFO", 200},
		{"unauthorized", "GET", "/eufy/connection", "", "", "/eufy/connection", "WARN", 401},
		{"route parameter", "GET", "/oura/collections/private-value", testToken, "", "/oura/collections/:collection", "WARN", 404},
		{"unmatched", "GET", "/private-value", "", "", "<unmatched>", "WARN", 404},
		{"method", "DELETE", "/test/error", "", "", "/test/error", "WARN", 405},
		{"body limit", "POST", "/oura/sync", testToken, strings.Repeat("x", 65537), "/oura/sync", "WARN", 413},
		{"upstream failure", "GET", "/eufy/latest-weight", testToken, "", "/eufy/latest-weight", "ERROR", 503},
		{"error", "GET", "/test/error", "", "", "/test/error", "ERROR", 500},
		{"panic", "GET", "/test/panic", "", "", "/test/panic", "ERROR", 500},
		{"failed probe", "GET", "/readyz", "", "", "/readyz", "ERROR", 500},
		{"committed response", "GET", "/test/committed", "", "", "/test/committed", "INFO", 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			api := (&Server{Store: &Store{Config: testConfig()}, Log: slog.New(slog.NewJSONHandler(&logs, nil))}).Handler()
			api.GET("/test/error", func(*echo.Context) error { return errors.New("private-error-value") })
			api.GET("/test/panic", func(*echo.Context) error { panic("private-panic-value") })
			api.GET("/test/committed", func(c *echo.Context) error {
				if err := c.JSON(202, map[string]string{"status": "accepted"}); err != nil {
					return err
				}
				return errors.New("private-error-after-write")
			})
			req := httptest.NewRequest(tc.method, "https://lifeblood.example"+tc.path, strings.NewReader(tc.body))
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			res := httptest.NewRecorder()
			api.ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", res.Code, tc.status, res.Body)
			}
			var record struct {
				Level         string  `json:"level"`
				Message       string  `json:"msg"`
				Method        string  `json:"method"`
				Route         string  `json:"route"`
				Status        int     `json:"status"`
				DurationMS    float64 `json:"duration_ms"`
				ResponseBytes int     `json:"response_bytes"`
			}
			decoder := json.NewDecoder(&logs)
			if err := decoder.Decode(&record); err != nil {
				t.Fatal(err)
			}
			if record.Message != "request completed" || record.Level != tc.level || record.Method != tc.method || record.Route != tc.route || record.Status != res.Code || record.DurationMS < 0 || record.ResponseBytes != res.Body.Len() {
				t.Fatalf("unexpected request log: %+v", record)
			}
			if decoder.Decode(new(any)) != io.EOF {
				t.Fatal("expected exactly one request log")
			}
			// Handling an error for logging must not write the JSON error response twice.
			body := json.NewDecoder(res.Body)
			if err := body.Decode(new(any)); err != nil || body.Decode(new(any)) != io.EOF {
				t.Fatal("expected exactly one JSON response", err)
			}
		})
	}
}

func TestRequestLogsExcludePrivateValues(t *testing.T) {
	var logs bytes.Buffer
	api := (&Server{Store: &Store{Config: testConfig()}, Log: slog.New(slog.NewJSONHandler(&logs, nil))}).Handler()
	api.POST("/test/:id", func(c *echo.Context) error {
		return c.JSON(200, map[string]string{"health": "private-response-value"})
	})
	api.GET("/test/error", func(*echo.Context) error { return errors.New("private-error-value") })
	api.GET("/test/panic", func(*echo.Context) error { panic("private-panic-value") })
	for _, path := range []string{"/test/private-path-value", "/private-unmatched-value", "/oura/oauth/callback", "/test/error", "/test/panic"} {
		method := http.MethodGet
		if path == "/test/private-path-value" {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, "https://lifeblood.example"+path+"?code=private-code-value&state=private-state-value&token="+testToken, strings.NewReader("private-body-value"))
		for _, header := range []string{"Cookie", "Referer", "User-Agent", "X-Request-Id", "X-Forwarded-For"} {
			req.Header.Set(header, "private-header-value")
		}
		req.Header.Set("Authorization", "Bearer "+testToken)
		api.ServeHTTP(httptest.NewRecorder(), req)
	}
	if strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), testToken) {
		t.Fatalf("private request/response/error value in logs: %s", &logs)
	}
	allowed := map[string]bool{"time": true, "level": true, "msg": true, "method": true, "route": true, "status": true, "duration_ms": true, "response_bytes": true}
	decoder := json.NewDecoder(&logs)
	for range 5 {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		for key := range record {
			if !allowed[key] {
				t.Fatalf("unexpected log field %q", key)
			}
		}
	}
}

func TestRequestLogsQuietProbesAndRateLimits(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	probes := echo.New()
	probes.Use(requestLogger(log))
	for _, path := range []string{"/healthz", "/readyz"} {
		probes.GET(path, func(c *echo.Context) error { return c.JSON(200, map[string]string{"status": "ok"}) })
	}
	for range 40 {
		for _, path := range []string{"/healthz", "/readyz"} {
			if res := testRequest(probes, "GET", path, ""); res.Code != 200 {
				t.Fatalf("probe status = %d", res.Code)
			}
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("healthy probes logged: %s", &logs)
	}
	var res *httptest.ResponseRecorder
	api := (&Server{Store: &Store{Config: testConfig()}, Log: log}).Handler()
	for range 31 {
		res = testRequest(api, "GET", "/eufy/connection", "")
	}
	if res.Code != 429 || !strings.Contains(logs.String(), `"status":429`) {
		t.Fatalf("rate limit not logged: %d %s", res.Code, &logs)
	}
}
