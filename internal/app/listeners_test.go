package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func requestAt(handler http.Handler, method, url, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, url, nil)
	req.RemoteAddr = "203.0.113.10:12345"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

func TestPublicListenerNeverRegistersDataOrControlRoutes(t *testing.T) {
	store := &Store{Config: testConfig()}
	server := &Server{Store: store, Oura: NewOura(store), Log: testLog()}
	public := server.PublicHandler()
	// All current retrieval and control routes must remain absent, even with auth.
	for _, route := range []struct{ method, path string }{
		{"GET", "/oura/connection"}, {"GET", "/oura/daily"}, {"GET", "/oura/sleep"},
		{"GET", "/oura/collections/sleep"}, {"GET", "/oura/sync"}, {"POST", "/oura/sync"},
		{"GET", "/eufy/connection"}, {"GET", "/eufy/members"}, {"GET", "/eufy/devices"},
		{"GET", "/eufy/latest-weight"}, {"GET", "/eufy/measurements"},
	} {
		for _, token := range []string{"", testToken} {
			res := requestAt(public, route.method, store.Config.PublicURL+route.path, token)
			if res.Code != http.StatusNotFound {
				t.Fatalf("public %s %s returned %d", route.method, route.path, res.Code)
			}
		}
	}
	private := server.PrivateHandler()
	if res := testRequest(private, "GET", "/eufy/connection", testToken); res.Code != http.StatusOK {
		t.Fatalf("authenticated private request returned %d", res.Code)
	}
	if res := testRequest(private, "GET", "/eufy/connection", ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("private request without bearer returned %d", res.Code)
	}
}

func TestPublicIntegrationsStayPublicAndProbesStayOffPrivateListener(t *testing.T) {
	store := &Store{Config: testConfig()}
	store.Config.OuraClientID = ""
	server := &Server{Store: store, Oura: NewOura(store), Log: testLog()}
	public, private := server.PublicHandler(), server.PrivateHandler()
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/oura/webhooks?verification_token=" + store.Config.WebhookSecret + "&challenge=synthetic-challenge", 200},
		{"GET", "/oura/webhooks", 401},
		{"POST", "/oura/webhooks", 503},
		{"GET", "/oura/oauth/callback", 503},
		{"GET", "/healthz", 200},
	} {
		res := requestAt(public, tc.method, store.Config.PublicURL+tc.path, "")
		if res.Code != tc.status {
			t.Fatalf("public %s %s returned %d, want %d", tc.method, tc.path, res.Code, tc.status)
		}
		if strings.Contains(tc.path, "challenge=") && !strings.Contains(res.Body.String(), "synthetic-challenge") {
			t.Fatal("webhook challenge was not echoed")
		}
		if res := testRequest(private, tc.method, tc.path, testToken); res.Code != 404 {
			t.Fatalf("private listener exposed %s: %d", tc.path, res.Code)
		}
	}
	if res := testRequest(private, "GET", "/readyz", testToken); res.Code != 404 {
		t.Fatalf("private listener exposed readiness probe: %d", res.Code)
	}
}

func TestListenerHostnamesCannotBeInterchangedOrForwarded(t *testing.T) {
	store := &Store{Config: testConfig()}
	server := &Server{Store: store, Oura: NewOura(store), Log: testLog()}
	for _, tc := range []struct {
		handler http.Handler
		url     string
	}{
		{server.PublicHandler(), testPrivateURL + "/oura/webhooks"},
		{server.PrivateHandler(), store.Config.PublicURL + "/eufy/connection"},
		{server.PublicHandler(), "https://unexpected.example/oura/webhooks"},
		{server.PrivateHandler(), "https://unexpected.example/eufy/connection"},
	} {
		req := httptest.NewRequest("GET", tc.url, nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("X-Forwarded-Host", "lifeblood.tailnet.example")
		req.Header.Set("Forwarded", "host=lifeblood.tailnet.example;proto=https")
		res := httptest.NewRecorder()
		tc.handler.ServeHTTP(res, req)
		if res.Code != 400 || !strings.Contains(res.Body.String(), "invalid_host") {
			t.Fatalf("unexpected hostname accepted: %s returned %d", tc.url, res.Code)
		}
	}
}

func TestPrivateOriginAndListenerConfiguration(t *testing.T) {
	values := map[string]string{
		"PUBLIC_BASE_URL": "https://lifeblood.example", "PRIVATE_BASE_URL": testPrivateURL,
		"DATABASE_URL": "postgres://test@localhost/test", "API_TOKEN": testToken,
		"TOKEN_ENCRYPTION_KEY": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	}
	get := func(k string) string { return values[k] }
	for _, origin := range []string{"", "http://private.example", "https://private.example/", "https://private.example/path", "https://private.example?query=1", "https://user@private.example", "https://private.example#fragment"} {
		values["PRIVATE_BASE_URL"] = origin
		if _, err := ParseConfig(get); err == nil {
			t.Fatalf("invalid private origin accepted: %q", origin)
		}
	}
	values["PRIVATE_BASE_URL"] = testPrivateURL
	values["LISTEN_ADDRESS"], values["PRIVATE_LISTEN_ADDRESS"] = ":9080", ":9082"
	cfg, err := ParseConfig(get)
	if err != nil || cfg.Address != ":9080" || cfg.PrivateAddress != ":9082" {
		t.Fatalf("separate listener configuration failed: %v", err)
	}
	values["PRIVATE_LISTEN_ADDRESS"] = values["LISTEN_ADDRESS"]
	if _, err := ParseConfig(get); err == nil {
		t.Fatal("shared listen address accepted")
	}
}
