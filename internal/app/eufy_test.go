package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sam-bee/lifeblood/internal/eufylife"
)

type fixtureEufy struct {
	calls    int
	snapshot eufylife.Snapshot
	err      error
}

func (f *fixtureEufy) Snapshot(context.Context) (eufylife.Snapshot, error) {
	f.calls++
	return f.snapshot, f.err
}
func (f *fixtureEufy) Status() map[string]any { return map[string]any{"configured": true} }

func TestEufyRoutesFilterMembersAndPaginateLocally(t *testing.T) {
	weight := 72.4
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	f := &fixtureEufy{snapshot: eufylife.Snapshot{Members: []eufylife.Member{{ID: "owner", Default: true}, {ID: "family"}},
		Measurements: []eufylife.Measurement{
			{ID: "family", CustomerID: "family", WeightKG: &weight, Timestamp: now.Add(time.Hour)},
			{ID: "today", CustomerID: "owner", WeightKG: &weight, Timestamp: now},
			{ID: "yesterday", CustomerID: "owner", WeightKG: &weight, Timestamp: now.Add(-24 * time.Hour)},
		}}}
	s := &Store{Config: testConfig()}
	api := (&Server{Store: s, Oura: NewOura(s), Eufy: f, Log: testLog()}).Handler()
	w := testRequest(api, "GET", "/eufy/latest-weight", testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"today"`) || strings.Contains(w.Body.String(), `"id":"family"`) {
		t.Fatalf("wrong default member: %s", w.Body.String())
	}
	w = testRequest(api, "GET", "/eufy/measurements?customer_id=owner&limit=1", testToken)
	var response struct {
		Total int                    `json:"total"`
		Next  *int                   `json:"next_offset"`
		Data  []eufylife.Measurement `json:"data"`
	}
	if json.Unmarshal(w.Body.Bytes(), &response) != nil || response.Total != 2 || response.Next == nil || *response.Next != 1 || response.Data[0].ID != "today" {
		t.Fatalf("bad pagination: %s", w.Body.String())
	}
	w = testRequest(api, "GET", "/eufy/measurements?customer_id=owner&end=2026-11-06", testToken)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"id":"today"`) || !strings.Contains(w.Body.String(), `"id":"yesterday"`) {
		t.Fatal("end date was not exclusive")
	}
	before := f.calls
	for _, path := range []string{"/eufy/measurements?limit=501", "/eufy/measurements?offset=-1", "/eufy/measurements?start=invalid", "/eufy/measurements?start=2026-11-06&end=2026-11-05"} {
		if w := testRequest(api, "GET", path, testToken); w.Code != 400 {
			t.Fatalf("invalid filter accepted: %s", path)
		}
	}
	if f.calls != before {
		t.Fatal("invalid filters fetched cloud data")
	}
	f.snapshot.Members[0].Default = false
	if w := testRequest(api, "GET", "/eufy/latest-weight", testToken); w.Code != 400 {
		t.Fatal("ambiguous member chosen silently")
	}
}

func TestEufyDisabledAndConfigurationPair(t *testing.T) {
	s := &Store{Config: testConfig()}
	api := (&Server{Store: s, Oura: NewOura(s), Log: testLog()}).Handler()
	if w := testRequest(api, "GET", "/eufy/connection", testToken); w.Code != 200 || !strings.Contains(w.Body.String(), "not_configured") {
		t.Fatal("missing disabled status")
	}
	if w := testRequest(api, "GET", "/eufy/latest-weight", testToken); w.Code != 503 || !strings.Contains(w.Body.String(), "eufy_not_configured") {
		t.Fatal("missing configuration error")
	}
	values := map[string]string{"PUBLIC_BASE_URL": "https://lifeblood.example", "DATABASE_URL": "postgres://test@localhost/test", "API_TOKEN": testToken, "TOKEN_ENCRYPTION_KEY": base64.StdEncoding.EncodeToString(make([]byte, 32))}
	for _, key := range []string{"EUFYLIFE_EMAIL", "EUFYLIFE_PASSWORD"} {
		values[key] = "value"
		if _, err := ParseConfig(func(k string) string { return values[k] }); err == nil {
			t.Fatal("partial credentials accepted")
		}
		delete(values, key)
	}
	values["EUFYLIFE_EMAIL"] = "synthetic@example.com"
	values["EUFYLIFE_PASSWORD"] = "password with spaces"
	if c, err := ParseConfig(func(k string) string { return values[k] }); err != nil || c.EufyPassword != values["EUFYLIFE_PASSWORD"] {
		t.Fatal("valid credentials changed or rejected")
	}
}
