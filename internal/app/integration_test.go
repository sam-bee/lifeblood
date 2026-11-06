package app

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

func integrationStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + hex.EncodeToString(digest(randomToken())[:8])
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pc.ConnConfig.RuntimeParams["search_path"] = schema
	pc.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pc.MaxConns = 12
	db, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		admin.Close()
	})
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal("migration not idempotent:", err)
	}
	cfg := testConfig()
	vault, _ := NewVault(cfg.EncryptionKey)
	return &Store{DB: db, Config: cfg, Vault: vault}
}

func seedConnection(t *testing.T, s *Store, expired bool) Connection {
	t.Helper()
	expiry := time.Now().Add(time.Hour)
	if expired {
		expiry = time.Now().Add(-time.Hour)
	}
	b, _ := json.Marshal(&oauth2.Token{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", TokenType: "Bearer", Expiry: expiry})
	if _, err := s.DB.Exec(context.Background(), `INSERT INTO oura_connections(oura_user_id,tokens,token_expiry) VALUES('synthetic-user',$1,$2)`, s.Vault.Seal(b, "oura-tokens"), expiry); err != nil {
		t.Fatal(err)
	}
	c, err := s.Connection(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mockOura(t *testing.T, s *Store, handler http.HandlerFunc) *Oura {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	o := NewOura(s)
	o.BaseURL = server.URL
	o.OAuth.Endpoint.TokenURL = server.URL + "/oauth/token"
	return o
}

func TestIntegrationWebhookInboxAndLeaseRecovery(t *testing.T) {
	s := integrationStore(t)
	c := seedConnection(t, s, false)
	ctx := context.Background()
	e := Webhook{Event: "update", Collection: "sleep", ObjectID: "one", UserID: c.OuraUserID, EventTime: time.Now().UTC()}
	b, _ := json.Marshal(e)
	for range 2 {
		if err := s.ReceiveWebhook(ctx, b, e); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&count); err != nil || count != 1 {
		t.Fatal("retry not deduplicated", count, err)
	}
	first, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("job claimed twice")
	}
	if _, err = s.DB.Exec(ctx, `UPDATE jobs SET leased_until=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	again, err := s.Claim(ctx)
	if err != nil || again.ID != first.ID || again.Attempts != 2 {
		t.Fatal("lease not recovered", err)
	}
	e.EventTime = e.EventTime.Add(time.Second)
	b, _ = json.Marshal(e)
	if err = s.ReceiveWebhook(ctx, b, e); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&count); err != nil || count != 2 {
		t.Fatal("later update deduplicated")
	}
	e.UserID = "other-user"
	b, _ = json.Marshal(e)
	if err = s.ReceiveWebhook(ctx, b, e); err != nil {
		t.Fatal(err)
	}
	if err = s.DB.QueryRow(ctx, "SELECT count(*) FROM jobs").Scan(&count); err != nil || count != 2 {
		t.Fatal("unknown account queued")
	}
}

func TestIntegrationRefreshIsSerializedAndPersisted(t *testing.T) {
	s := integrationStore(t)
	c := seedConnection(t, s, true)
	var calls atomic.Int32
	o := mockOura(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		calls.Add(1)
		r.ParseForm()
		if r.Form.Get("refresh_token") != "synthetic-refresh" {
			t.Error("wrong refresh token")
		}
		id, secret, ok := r.BasicAuth()
		if !ok || id != s.Config.OuraClientID || secret != s.Config.OuraClientSecret {
			t.Error("missing application credentials")
		}
		if r.UserAgent() == "" {
			t.Error("missing identifying header")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`)
	})
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := o.accessToken(context.Background(), c, "")
			if err != nil || token != "new-access" {
				t.Errorf("token refresh: %q %v", token, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("single-use refresh token reused %d times", calls.Load())
	}
	updated, err := s.Connection(context.Background())
	if err != nil || updated.Status != "active" {
		t.Fatal(err)
	}
	plain, err := s.Vault.Open(updated.Tokens, "oura-tokens")
	if err != nil || !strings.Contains(string(plain), "new-refresh") {
		t.Fatal("replacement not persisted")
	}
}

func TestIntegrationAmbiguousRefreshRequiresRelinkWithoutRetry(t *testing.T) {
	s := integrationStore(t)
	c := seedConnection(t, s, true)
	var calls atomic.Int32
	o := mockOura(t, s, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) })
	for range 2 {
		if _, err := o.accessToken(context.Background(), c, ""); err == nil {
			t.Fatal("refresh failure hidden")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("consumed refresh token retried")
	}
	updated, _ := s.Connection(context.Background())
	if updated.Status != "reconnect_required" {
		t.Fatal("uncertain refresh not recorded")
	}
}

func TestIntegrationDeletedRecordCannotBeResurrectedByOlderEvent(t *testing.T) {
	s := integrationStore(t)
	c := seedConnection(t, s, false)
	var fetches atomic.Int32
	o := mockOura(t, s, func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		fmt.Fprintf(w, `{"id":"record","day":%q}`, time.Now().UTC().Format(time.DateOnly))
	})
	w := &Worker{Store: s, Oura: o, Log: testLog()}
	ctx := context.Background()
	when := time.Now().UTC().Add(-time.Second)
	e := Webhook{Event: "delete", Collection: "sleep", ObjectID: "record", UserID: c.OuraUserID, EventTime: when}
	if err := w.syncEvent(ctx, c, e); err != nil {
		t.Fatal(err)
	}
	e.Event = "update"
	e.EventTime = when.Add(-time.Minute)
	if err := w.syncEvent(ctx, c, e); err != nil {
		t.Fatal(err)
	}
	if fetches.Load() != 0 {
		t.Fatal("older event unnecessarily fetched")
	}
	e.EventTime = when
	if err := w.syncEvent(ctx, c, e); err != nil {
		t.Fatal(err)
	}
	var deleted bool
	var raw []byte
	if err := s.DB.QueryRow(ctx, `SELECT deleted,payload FROM oura_documents WHERE source_id='record'`).Scan(&deleted, &raw); err != nil || !deleted || raw != nil {
		t.Fatal("deletion was undone", err)
	}
}

func TestIntegrationPaginationBoundariesAndMissedDeletions(t *testing.T) {
	s := integrationStore(t)
	c := seedConnection(t, s, false)
	ctx := context.Background()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	end := day.AddDate(0, 0, 1)
	if _, err := s.DB.Exec(ctx, `INSERT INTO oura_documents(connection_id,collection,source_id,source_day,payload) VALUES($1,'sleep','missing',$2,'{}')`, c.ID, day); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	o := mockOura(t, s, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("next_token") == "" {
			fmt.Fprintf(w, `{"data":[{"id":"present","day":%q}],"next_token":"page-2"}`, day.Format(time.DateOnly))
		} else {
			fmt.Fprintf(w, `{"data":[{"id":"end-boundary","day":%q}],"next_token":null}`, end.Format(time.DateOnly))
		}
	})
	w := &Worker{Store: s, Oura: o, Log: testLog()}
	if err := w.syncRange(ctx, c, RangeJob{"sleep", day.Format(time.DateOnly), end.Format(time.DateOnly)}); err != nil {
		t.Fatal(err)
	}
	docs, err := s.Documents(ctx, c.ID, []string{"sleep"}, day, end, "")
	if err != nil || len(docs) != 1 || docs[0].ID != "present" || calls.Load() != 2 {
		t.Fatal("incorrect paginated range", len(docs), err)
	}
	var deleted bool
	var raw []byte
	if err = s.DB.QueryRow(ctx, `SELECT deleted,payload FROM oura_documents WHERE source_id='missing'`).Scan(&deleted, &raw); err != nil || !deleted || raw != nil {
		t.Fatal("missed deletion not reconciled", err)
	}
}

func TestIntegrationMalformedResponseDoesNotDeleteStoredData(t *testing.T) {
	s := integrationStore(t)
	c := seedConnection(t, s, false)
	ctx := context.Background()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	if _, err := s.DB.Exec(ctx, `INSERT INTO oura_documents(connection_id,collection,source_id,source_day,payload) VALUES($1,'sleep','kept',$2,'{}')`, c.ID, day); err != nil {
		t.Fatal(err)
	}
	o := mockOura(t, s, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"unexpected":"response"}`) })
	w := &Worker{Store: s, Oura: o, Log: testLog()}
	if err := w.syncRange(ctx, c, RangeJob{"sleep", day.Format(time.DateOnly), day.AddDate(0, 0, 1).Format(time.DateOnly)}); err == nil {
		t.Fatal("missing data array accepted")
	}
	var deleted bool
	if err := s.DB.QueryRow(ctx, `SELECT deleted FROM oura_documents WHERE source_id='kept'`).Scan(&deleted); err != nil || deleted {
		t.Fatal("stored data deleted")
	}
}

func TestIntegrationDeleteInvalidatesInflightWorkAndClearsHealthData(t *testing.T) {
	s := integrationStore(t)
	c := seedConnection(t, s, false)
	started, release := make(chan struct{}), make(chan struct{})
	o := mockOura(t, s, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/oauth/revoke") {
			w.WriteHeader(200)
			return
		}
		close(started)
		<-release
		fmt.Fprintf(w, `{"id":"record","day":%q}`, time.Now().UTC().Format(time.DateOnly))
	})
	w := &Worker{Store: s, Oura: o, Log: testLog()}
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- w.syncEvent(ctx, c, Webhook{Event: "update", Collection: "sleep", ObjectID: "record", UserID: c.OuraUserID, EventTime: time.Now()})
	}()
	<-started
	if err := o.Disconnect(ctx, true); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("stale worker committed after deletion")
	}
	var count int
	if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM oura_documents").Scan(&count); err != nil || count != 0 {
		t.Fatal("health data remains")
	}
	conn, err := s.Connection(ctx)
	if err != nil || conn.Status != "disconnected" || len(conn.Tokens) != 0 {
		t.Fatal("credentials not removed")
	}
}

func TestIntegrationAuthorizationIsSingleUseAndPinsOneAccount(t *testing.T) {
	s := integrationStore(t)
	var person atomic.Value
	person.Store("first-person")
	o := mockOura(t, s, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth/token" {
			fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","token_type":"Bearer","expires_in":3600}`)
			return
		}
		if r.URL.Path == "/v2/usercollection/personal_info" {
			fmt.Fprintf(w, `{"id":%q}`, person.Load())
			return
		}
		w.WriteHeader(404)
	})
	ctx := context.Background()
	link, err := o.BeginLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state := strings.Split(strings.Split(link, "state=")[1], "&")[0]
	if err = o.CompleteLink(ctx, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err = o.CompleteLink(ctx, state, "code"); err == nil {
		t.Fatal("authorization replay accepted")
	}
	person.Store("second-person")
	link, err = o.BeginLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state = strings.Split(strings.Split(link, "state=")[1], "&")[0]
	if err = o.CompleteLink(ctx, state, "code"); err == nil {
		t.Fatal("different Oura account silently replaced pinned account")
	}
	conn, err := s.Connection(ctx)
	if err != nil || conn.OuraUserID != "first-person" {
		t.Fatal("wrong account linked")
	}
}

func TestIntegrationSubscriptionCreationAndRenewalWithProductionExpiry(t *testing.T) {
	s := integrationStore(t)
	var mu sync.Mutex
	var subscriptions []map[string]string
	created, renewed := 0, 0
	expiry := time.Now().UTC().Add(90 * 24 * time.Hour).Format("2006-01-02T15:04:05.999999")
	o := mockOura(t, s, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-client-id") != s.Config.OuraClientID || r.Header.Get("x-client-secret") != s.Config.OuraClientSecret {
			t.Error("missing application credentials")
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if subscriptions == nil {
				fmt.Fprint(w, "[]")
			} else {
				json.NewEncoder(w).Encode(subscriptions)
			}
		case http.MethodPost:
			var input map[string]string
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
			}
			if input["verification_token"] != s.Config.WebhookSecret || input["callback_url"] != s.Config.PublicURL+"/oura/webhooks" {
				t.Error("incorrect webhook configuration")
			}
			created++
			sub := map[string]string{"id": fmt.Sprintf("subscription-%d", created), "callback_url": input["callback_url"],
				"data_type": input["data_type"], "event_type": input["event_type"], "expiration_time": expiry}
			subscriptions = append(subscriptions, sub)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(sub)
		case http.MethodPut:
			id := strings.TrimPrefix(r.URL.Path, "/v2/webhook/subscription/renew/")
			for _, sub := range subscriptions {
				if sub["id"] == id {
					renewed++
					sub["expiration_time"] = expiry
					json.NewEncoder(w).Encode(sub)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	ctx := context.Background()
	if err := o.ReconcileSubscriptions(ctx); err != nil {
		t.Fatal(err)
	}
	if err := o.ReconcileSubscriptions(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if created != 12 {
		t.Fatalf("expected 12 subscriptions without duplicates, got %d", created)
	}
	subscriptions[0]["expiration_time"] = time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05.999999")
	mu.Unlock()
	if err := o.ReconcileSubscriptions(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if renewed != 1 || created != 12 {
		t.Fatalf("unexpected renewal/create counts: %d/%d", renewed, created)
	}
	var count int
	if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM webhook_subscriptions WHERE expires_at>now()+interval '1 day'").Scan(&count); err != nil || count != 12 {
		t.Fatalf("subscriptions not persisted with valid expiration: %d %v", count, err)
	}
}
