package eufylife

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const recordsJSON = `[{"id":"older","customer_id":"owner","create_time":1700000000,"update_time":1800000000,"scale_data":{"weight":750,"body_fat":20.5,"muscle_mass":55.2,"body_age":40}},
{"id":"newer","customer_id":"owner","create_time":1700000100,"scale_data":{"weight":740,"body_fat":0}},
{"id":"other","customer_id":"family","create_time":1700000200,"scale_data":{"weight":600}},
{"customer_id":"owner","scale_data":{"weight":100}}, {"create_time":1700000300}]`

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New("synthetic@example.com", "synthetic-password")
	c.baseURL = srv.URL
	return c
}

func loginResponse(w http.ResponseWriter, token string) {
	fmt.Fprintf(w, `{"res_code":1,"access_token":%q,"user_id":"account","expires_in":2592000,"customers":[{"id":"owner","name":"Owner","default":true}],"devices":[{"id":"scale","name":"P3","wifi":{"password":"must-not-escape"}}],"refresh_token":"must-not-escape"}`, token)
}

func TestSharedCacheConcurrencyExpiryAndTokenReuse(t *testing.T) {
	var logins, reads atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/user/v2/email/login" {
			logins.Add(1)
			var input map[string]string
			if json.NewDecoder(r.Body).Decode(&input) != nil || input["email"] != "synthetic@example.com" || input["password"] != "synthetic-password" || r.Header.Get("Country") != "US" {
				t.Error("incorrect login protocol")
			}
			loginResponse(w, "synthetic-token")
			return
		}
		reads.Add(1)
		if r.Header.Get("Token") != "synthetic-token" || r.Header.Get("Uid") != "account" {
			t.Error("incorrect account authentication")
		}
		fmt.Fprint(w, `{"res_code":1,"data":`+recordsJSON+`}`)
	})
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			v, err := c.Snapshot(context.Background())
			if err != nil || len(v.Measurements) != 3 || v.SkippedRecords != 2 {
				t.Errorf("unexpected snapshot: %v", err)
				return
			}
			if v.Measurements[0].ID != "other" || v.Measurements[1].ID != "newer" {
				t.Error("sorted by update time instead of weigh-in time")
			}
			if *v.Measurements[1].WeightKG != 74 || v.Measurements[1].BodyFatPercent != nil || *v.Measurements[2].MuscleMassKG != 55.2 {
				t.Error("incorrect units or missing values")
			}
			if v.Cache.ExpiresAt.Sub(v.Cache.FetchedAt) != CacheTTL {
				t.Error("incorrect TTL")
			}
			b, _ := json.Marshal(v)
			if strings.Contains(string(b), "must-not-escape") {
				t.Error("upstream credentials exposed")
			}
		})
	}
	wg.Wait()
	if logins.Load() != 1 || reads.Load() != 1 {
		t.Fatalf("cache stampede: %d logins, %d reads", logins.Load(), reads.Load())
	}
	now = now.Add(CacheTTL - time.Nanosecond)
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 1 {
		t.Fatal("refreshed before expiry")
	}
	now = now.Add(time.Nanosecond)
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 1 || reads.Load() != 2 {
		t.Fatal("token was not reused at cache expiry")
	}
	now = now.Add(31 * 24 * time.Hour)
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 2 || reads.Load() != 3 {
		t.Fatal("expired session was not renewed")
	}
}

func TestFailuresAreRedactedAndCooledDown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		login  bool
		retry  string
	}{
		{"bad_password", 200, `{"res_code":0,"message":"sensitive-password"}`, true, ""},
		{"html", 200, `<html>sensitive-token</html>`, false, ""},
		{"missing_data", 200, `{"res_code":1}`, false, ""},
		{"missing_code", 200, `{"data":[]}`, false, ""},
		{"null_data", 200, `{"res_code":1,"data":null}`, false, ""},
		{"rate_limit", 429, `sensitive-token`, false, "900"},
		{"redirect", 302, `sensitive-token`, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/user/v2/email/login" && !tc.login {
					loginResponse(w, "token")
					return
				}
				calls.Add(1)
				w.Header().Set("Retry-After", tc.retry)
				w.Header().Set("Location", "https://example.invalid/token-leak")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			now := time.Now()
			c.now = func() time.Time { return now }
			for range 10 {
				v, err := c.Snapshot(context.Background())
				if err == nil || strings.Contains(err.Error(), "sensitive") || len(v.Measurements) != 0 {
					t.Fatal("failure was hidden or leaked payload")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("failure retried without cooldown")
			}
			delay := CacheTTL
			if tc.retry != "" {
				delay = 15 * time.Minute
			}
			if got := c.Status()["retry_at"].(time.Time); !got.Equal(now.Add(delay)) {
				t.Fatal("Retry-After ignored")
			}
			now = now.Add(delay)
			_, _ = c.Snapshot(context.Background())
			if calls.Load() != 2 {
				t.Fatal("did not retry after cooldown")
			}
		})
	}
}

func TestRejectedTokenRelogsOnceAndArrayResponseWorks(t *testing.T) {
	var logins atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/user/v2/email/login" {
			n := logins.Add(1)
			loginResponse(w, fmt.Sprint(n))
			return
		}
		if r.Header.Get("Token") == "1" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, recordsJSON)
	})
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 2 {
		t.Fatal("rejected token was not replaced")
	}
}

func TestCanceledWaiterDoesNotCancelSharedRefresh(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/user/v2/email/login" {
			loginResponse(w, "token")
			return
		}
		close(started)
		<-release
		fmt.Fprint(w, `[]`)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Snapshot(ctx); done <- err }()
	<-started
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatal("canceled request did not return promptly")
	}
	close(release)
	if _, err := c.Snapshot(context.Background()); err != nil {
		t.Fatal(err)
	}
}
