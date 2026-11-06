package app

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testToken = "local-test-api-token-with-32-bytes-minimum"

func testConfig() Config {
	return Config{PublicURL: "https://lifeblood.example", APITokenHash: digest(testToken), EncryptionKey: bytes.Repeat([]byte{1}, 32),
		OuraClientID: "test-client", OuraClientSecret: strings.Repeat("s", 32), WebhookSecret: strings.Repeat("w", 32), HistoryDays: 90, RetentionDays: 365}
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestConfigurationRequiresAPIAuthButNoSSO(t *testing.T) {
	values := map[string]string{"PUBLIC_BASE_URL": "https://lifeblood.example", "DATABASE_URL": "postgres://test@localhost/test",
		"API_TOKEN": testToken, "TOKEN_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))}
	get := func(k string) string { return values[k] }
	if _, err := ParseConfig(get); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"API_TOKEN", "TOKEN_ENCRYPTION_KEY", "DATABASE_URL"} {
		old := values[name]
		delete(values, name)
		if _, err := ParseConfig(get); err == nil {
			t.Fatalf("missing %s accepted", name)
		}
		values[name] = old
	}
	values["PUBLIC_BASE_URL"] = "http://lifeblood.example"
	if _, err := ParseConfig(get); err == nil {
		t.Fatal("insecure public origin accepted")
	}
}

func TestVaultRejectsTamperingAndWrongPurpose(t *testing.T) {
	v, _ := NewVault(bytes.Repeat([]byte{2}, 32))
	plain := []byte("synthetic-refresh-token")
	one, two := v.Seal(plain, "oura-tokens"), v.Seal(plain, "oura-tokens")
	if bytes.Equal(one, two) {
		t.Fatal("nonce reused")
	}
	if b, err := v.Open(one, "oura-tokens"); err != nil || !bytes.Equal(b, plain) {
		t.Fatal("roundtrip failed")
	}
	if _, err := v.Open(one, "oura-link"); err == nil {
		t.Fatal("wrong purpose accepted")
	}
	one[len(one)-1] ^= 1
	if _, err := v.Open(one, "oura-tokens"); err == nil {
		t.Fatal("tampered value accepted")
	}
}

func sign(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write(body)
	return strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
}

func TestWebhookVerifiesExactBytesAndReplayWindow(t *testing.T) {
	now := time.Now()
	stamp := fmt.Sprint(now.Unix())
	body := []byte(`{"event_type": "update"}`)
	secret := "test-secret"
	sig := sign(secret, stamp, body)
	if !VerifyWebhook(secret, stamp, sig, body, now) {
		t.Fatal("valid signature rejected")
	}
	if VerifyWebhook(secret, stamp, sig, []byte(`{"event_type":"update"}`), now) {
		t.Fatal("changed bytes accepted")
	}
	if VerifyWebhook(secret, stamp, sig, body, now.Add(3*time.Hour)) {
		t.Fatal("stale delivery accepted")
	}
	if VerifyWebhook(secret, stamp, sig, body, now.Add(-10*time.Minute)) {
		t.Fatal("future timestamp accepted")
	}
	if VerifyWebhook(secret, stamp, "bad-hex", body, now) {
		t.Fatal("malformed signature accepted")
	}
}

func testRequest(handler http.Handler, method, path, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://lifeblood.example"+path, nil)
	r.RemoteAddr = "203.0.113.10:12345"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestEveryDataAndControlRouteRequiresBearer(t *testing.T) {
	s := &Store{Config: testConfig()}
	api := (&Server{Store: s, Oura: NewOura(s), Log: testLog()}).Handler()
	for _, route := range []struct{ method, path string }{{"GET", "/oura/connection"}, {"GET", "/oura/daily"}, {"GET", "/oura/sleep"},
		{"GET", "/oura/collections/sleep"}, {"POST", "/oura/sync"}, {"GET", "/oura/sync"}, {"DELETE", "/oura/data"}, {"DELETE", "/oura/connection"}} {
		for _, token := range []string{"", strings.Repeat("wrong", 8)} {
			w := testRequest(api, route.method, route.path, token)
			if w.Code != 401 {
				t.Fatalf("%s %s: %d %s", route.method, route.path, w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing no-store")
			}
		}
	}
	if w := testRequest(api, "GET", "/oura/collections/not-allowed", testToken); w.Code != 404 {
		t.Fatalf("valid bearer rejected: %d", w.Code)
	}
	if w := testRequest(api, "GET", "/oura/daily?token="+testToken, ""); w.Code != 401 {
		t.Fatal("query-string token accepted")
	}
	for _, path := range []string{"/", "/auth/login", "/oura/authorize", "/eufy/latest-weight"} {
		if w := testRequest(api, "GET", path, testToken); w.Code != 404 {
			t.Fatalf("unexpected interface at %s: %d", path, w.Code)
		}
	}
}

func TestRateLimitCannotBeBypassedByForgedForwardedHeaders(t *testing.T) {
	s := &Store{Config: testConfig()}
	api := (&Server{Store: s, Oura: NewOura(s), Log: testLog()}).Handler()
	for n := 0; n < 35; n++ {
		r := httptest.NewRequest("GET", "https://lifeblood.example/oura/daily", nil)
		r.RemoteAddr = "203.0.113.20:1234"
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", n))
		r.Header.Set("X-Real-IP", fmt.Sprintf("198.51.100.%d", n))
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r)
		if n == 34 && (w.Code != 429 || w.Header().Get("Retry-After") == "") {
			t.Fatalf("rate limit not enforced: %d", w.Code)
		}
	}
	if w := testRequest(api, "GET", "/healthz", ""); w.Code != 200 {
		t.Fatal("health probe rate-limited")
	}
}

func TestRangeBoundaries(t *testing.T) {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if _, _, err := parseRange(today.Format(time.DateOnly), today.Format(time.DateOnly), 90, 365); err == nil {
		t.Fatal("empty range accepted")
	}
	if _, _, err := parseRange("not-a-date", "", 90, 365); err == nil {
		t.Fatal("invalid date accepted")
	}
	if _, _, err := parseRange(today.AddDate(0, 0, -100).Format(time.DateOnly), today.Format(time.DateOnly), 90, 365); err == nil {
		t.Fatal("unbounded range accepted")
	}
	start, end, err := parseRange("", "", 90, 365)
	if err != nil || end.Sub(start) != 30*24*time.Hour {
		t.Fatal("unexpected default range")
	}
}
