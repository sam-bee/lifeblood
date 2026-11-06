package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

var Collections = []string{"daily_sleep", "daily_readiness", "daily_activity", "sleep"}

func collectionAllowed(s string) bool {
	for _, c := range Collections {
		if s == c {
			return true
		}
	}
	return false
}

type UpstreamError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *UpstreamError) Error() string { return fmt.Sprintf("oura_http_%d", e.Status) }

type userAgentTransport struct{ Base http.RoundTripper }

func (t userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", "Lifeblood/0.1 (+https://lifeblood.personalinfra.co.uk)")
	return t.Base.RoundTrip(r)
}

type Oura struct {
	Store   *Store
	OAuth   oauth2.Config
	HTTP    *http.Client
	BaseURL string
}

func NewOura(s *Store) *Oura {
	client := &http.Client{Timeout: 20 * time.Second, Transport: userAgentTransport{http.DefaultTransport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Oura{Store: s, HTTP: client, BaseURL: "https://api.ouraring.com", OAuth: oauth2.Config{
		ClientID: s.Config.OuraClientID, ClientSecret: s.Config.OuraClientSecret,
		RedirectURL: s.Config.PublicURL + "/oura/oauth/callback", Scopes: []string{"personal", "daily"},
		Endpoint: oauth2.Endpoint{AuthURL: "https://cloud.ouraring.com/oauth/authorize", TokenURL: "https://api.ouraring.com/oauth/token", AuthStyle: oauth2.AuthStyleInHeader}}}
}

type linkFlow struct {
	Verifier string `json:"verifier"`
	Epoch    int64  `json:"epoch"`
}

// BeginLink is used only by the operator's one-time CLI command, not an API
// endpoint. State is unguessable, expires in ten minutes and is consumed once.
func (o *Oura) BeginLink(ctx context.Context) (string, error) {
	if !o.Store.Config.OuraEnabled() {
		return "", errors.New("oura_not_configured")
	}
	state, verifier := randomToken(), oauth2.GenerateVerifier()
	var epoch int64
	if err := o.Store.DB.QueryRow(ctx, `SELECT oauth_epoch FROM lifeblood_state WHERE id=1`).Scan(&epoch); err != nil {
		return "", err
	}
	b, _ := json.Marshal(linkFlow{verifier, epoch})
	if _, err := o.Store.DB.Exec(ctx, `INSERT INTO oauth_flows VALUES($1,$2,now()+interval '10 minutes')`, digest(state), o.Store.Vault.Seal(b, "oura-link")); err != nil {
		return "", err
	}
	return o.OAuth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), nil
}

func (o *Oura) CompleteLink(ctx context.Context, state, code string) error {
	if len(state) != 43 {
		return errors.New("invalid_authorization")
	}
	var encrypted []byte
	if err := o.Store.DB.QueryRow(ctx, `DELETE FROM oauth_flows WHERE state_hash=$1 AND expires_at>now() RETURNING payload`, digest(state)).Scan(&encrypted); err != nil {
		return errors.New("invalid_authorization")
	}
	if code == "" || len(code) > 2048 {
		return errors.New("authorization_denied")
	}
	b, err := o.Store.Vault.Open(encrypted, "oura-link")
	if err != nil {
		return err
	}
	var flow linkFlow
	if err = json.Unmarshal(b, &flow); err != nil {
		return err
	}
	return o.Link(ctx, code, flow.Verifier, flow.Epoch)
}

func (o *Oura) request(ctx context.Context, method, path, token string, input, out any, management bool) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, o.BaseURL+path, body)
	if err != nil {
		return err
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if management {
		r.Header.Set("x-client-id", o.Store.Config.OuraClientID)
		r.Header.Set("x-client-secret", o.Store.Config.OuraClientSecret)
	}
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.HTTP.Do(r)
	if err != nil {
		return errors.New("oura_transport_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		delay := time.Duration(0)
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			delay = time.Duration(min(seconds, 86400)) * time.Second
		}
		return &UpstreamError{resp.StatusCode, delay}
	}
	if out == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
	if err != nil {
		return errors.New("oura_read_failed")
	}
	if len(b) > 8*1024*1024 {
		return errors.New("oura_response_too_large")
	}
	if err = json.Unmarshal(b, out); err != nil {
		return errors.New("oura_invalid_json")
	}
	return nil
}

func (o *Oura) Link(ctx context.Context, code, verifier string, epoch int64) error {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, o.HTTP)
	token, err := o.OAuth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return errors.New("oura_exchange_failed")
	}
	if token.RefreshToken == "" || token.AccessToken == "" || token.Expiry.IsZero() {
		return errors.New("oura_incomplete_token")
	}
	var person struct {
		ID string `json:"id"`
	}
	if err = o.request(ctx, http.MethodGet, "/v2/usercollection/personal_info", token.AccessToken, nil, &person, false); err != nil {
		return err
	}
	if person.ID == "" || len(person.ID) > 128 {
		return errors.New("oura_invalid_identity")
	}
	tx, err := o.Store.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialize the initial insert as well as relinks. A different Oura account
	// requires explicit data deletion before it can replace the pinned account.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(42719022)"); err != nil {
		return err
	}
	var currentEpoch int64
	if err = tx.QueryRow(ctx, `SELECT oauth_epoch FROM lifeblood_state WHERE id=1 FOR UPDATE`).Scan(&currentEpoch); err != nil {
		return err
	}
	if currentEpoch != epoch {
		return errors.New("authorization_cancelled")
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT oura_user_id FROM oura_connections WHERE singleton FOR UPDATE`).Scan(&existing)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if existing != "" && existing != person.ID {
		return errors.New("different_oura_account")
	}
	b, _ := json.Marshal(token)
	var c Connection
	err = tx.QueryRow(ctx, `INSERT INTO oura_connections(oura_user_id,tokens,token_expiry)
	 VALUES($1,$2,$3) ON CONFLICT(singleton) DO UPDATE SET tokens=EXCLUDED.tokens,token_expiry=EXCLUDED.token_expiry,
	 status='active',reconnect_reason=NULL,generation=oura_connections.generation+1 RETURNING id,generation`,
		person.ID, o.Store.Vault.Seal(b, "oura-tokens"), token.Expiry).Scan(&c.ID, &c.Generation)
	if err != nil {
		return err
	}
	for _, collection := range Collections {
		if _, err = tx.Exec(ctx, `INSERT INTO sync_state(connection_id,collection) VALUES($1,$2)
		 ON CONFLICT(connection_id,collection) DO UPDATE SET status='pending'`, c.ID, collection); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET status='cancelled',completed_at=now() WHERE connection_id=$1 AND status IN ('queued','running')`, c.ID); err != nil {
		return err
	}
	end := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	args := RangeJob{Start: end.AddDate(0, 0, -o.Store.Config.HistoryDays).Format(time.DateOnly), End: end.Format(time.DateOnly)}
	if err = enqueue(ctx, tx, c, "setup", args, "setup:"+randomToken()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Oura refresh tokens are single use. Persist the uncertain state before sending
// a refresh so a crash or lost response cannot silently retry a consumed token.
func (o *Oura) accessToken(ctx context.Context, c Connection, failedToken string) (string, error) {
	db, err := o.Store.DB.Acquire(ctx)
	if err != nil {
		return "", err
	}
	defer db.Release()
	if _, err = db.Exec(ctx, "SELECT pg_advisory_lock($1)", int64(50000000)+c.ID); err != nil {
		return "", err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := db.Exec(cleanup, "SELECT pg_advisory_unlock($1)", int64(50000000)+c.ID); err != nil {
			db.Conn().Close(cleanup)
		}
	}()
	var encrypted []byte
	var status string
	err = db.QueryRow(ctx, `SELECT tokens,status FROM oura_connections WHERE id=$1 AND generation=$2`, c.ID, c.Generation).Scan(&encrypted, &status)
	if err != nil || status != "active" {
		return "", errors.New("reconnect_required")
	}
	b, err := o.Store.Vault.Open(encrypted, "oura-tokens")
	if err != nil {
		return "", err
	}
	var token oauth2.Token
	if err = json.Unmarshal(b, &token); err != nil {
		return "", err
	}
	if token.Expiry.After(time.Now().Add(time.Minute)) && (failedToken == "" || token.AccessToken != failedToken) {
		return token.AccessToken, nil
	}
	result, err := db.Exec(ctx, `UPDATE oura_connections SET status='reconnect_required',reconnect_reason='refresh_in_progress'
	 WHERE id=$1 AND generation=$2 AND status='active'`, c.ID, c.Generation)
	if err != nil {
		return "", err
	}
	if result.RowsAffected() != 1 {
		return "", errors.New("reconnect_required")
	}
	token.Expiry = time.Now().Add(-time.Hour)
	refreshCtx := context.WithValue(ctx, oauth2.HTTPClient, o.HTTP)
	next, err := o.OAuth.TokenSource(refreshCtx, &token).Token()
	if err != nil || next == nil || next.RefreshToken == "" || next.RefreshToken == token.RefreshToken || next.AccessToken == "" || next.Expiry.IsZero() {
		return "", errors.New("reconnect_required")
	}
	b, _ = json.Marshal(next)
	result, err = db.Exec(ctx, `UPDATE oura_connections SET tokens=$3,token_expiry=$4,status='active',reconnect_reason=NULL
	 WHERE id=$1 AND generation=$2 AND status='reconnect_required' AND reconnect_reason='refresh_in_progress'`, c.ID, c.Generation, o.Store.Vault.Seal(b, "oura-tokens"), next.Expiry)
	if err != nil {
		return "", err
	}
	if result.RowsAffected() != 1 {
		return "", errors.New("reconnect_required")
	}
	return next.AccessToken, nil
}

func (o *Oura) Fetch(ctx context.Context, c Connection, path string, out any) error {
	token, err := o.accessToken(ctx, c, "")
	if err != nil {
		return err
	}
	err = o.request(ctx, http.MethodGet, path, token, nil, out, false)
	var upstream *UpstreamError
	if errors.As(err, &upstream) && upstream.Status == 401 {
		token, err = o.accessToken(ctx, c, token)
		if err != nil {
			return err
		}
		err = o.request(ctx, http.MethodGet, path, token, nil, out, false)
		if errors.As(err, &upstream) && upstream.Status == 401 {
			_, _ = o.Store.DB.Exec(ctx, `UPDATE oura_connections SET status='reconnect_required',reconnect_reason='authorization_rejected' WHERE id=$1 AND generation=$2 AND status='active'`, c.ID, c.Generation)
		}
	}
	return err
}

// Oura's production subscription responses can omit the UTC timezone suffix,
// although the schema describes an RFC 3339 date-time. Handle both forms.
type subscriptionTime struct{ time.Time }

func (t *subscriptionTime) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return errors.New("invalid_subscription_expiry")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		parsed, err = time.Parse("2006-01-02T15:04:05.999999999", value)
	}
	if err != nil {
		return errors.New("invalid_subscription_expiry")
	}
	t.Time = parsed
	return nil
}

type Subscription struct {
	ID         string           `json:"id"`
	Callback   string           `json:"callback_url"`
	Collection string           `json:"data_type"`
	Event      string           `json:"event_type"`
	Expiry     subscriptionTime `json:"expiration_time"`
}

func (o *Oura) ReconcileSubscriptions(ctx context.Context) error {
	var existing []Subscription
	if err := o.request(ctx, http.MethodGet, "/v2/webhook/subscription", "", nil, &existing, true); err != nil {
		return err
	}
	callback := o.Store.Config.PublicURL + "/oura/webhooks"
	for _, collection := range Collections {
		for _, event := range []string{"create", "update", "delete"} {
			var sub Subscription
			for _, candidate := range existing {
				if candidate.Callback == callback && candidate.Collection == collection && candidate.Event == event {
					sub = candidate
					break
				}
			}
			var err error
			if sub.ID == "" {
				input := map[string]string{"callback_url": callback, "verification_token": o.Store.Config.WebhookSecret, "data_type": collection, "event_type": event}
				err = o.request(ctx, http.MethodPost, "/v2/webhook/subscription", "", input, &sub, true)
			} else if sub.Expiry.Before(time.Now().Add(24 * time.Hour)) {
				err = o.request(ctx, http.MethodPut, "/v2/webhook/subscription/renew/"+url.PathEscape(sub.ID), "", nil, &sub, true)
			}
			if err != nil {
				return err
			}
			if sub.ID == "" || sub.Expiry.IsZero() {
				return errors.New("invalid_subscription")
			}
			if _, err = o.Store.DB.Exec(ctx, `INSERT INTO webhook_subscriptions VALUES($1,$2,$3,$4) ON CONFLICT(subscription_id) DO UPDATE SET expires_at=EXCLUDED.expires_at`, sub.ID, collection, event, sub.Expiry.Time); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *Oura) Disconnect(ctx context.Context, erase bool) error {
	// Invalidate queued and in-flight work before attempting network revocation.
	tx, err := o.Store.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(42719022)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE lifeblood_state SET oauth_epoch=oauth_epoch+1 WHERE id=1`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `DELETE FROM oauth_flows`); err != nil {
		return err
	}
	var c Connection
	err = tx.QueryRow(ctx, `SELECT id,tokens FROM oura_connections WHERE singleton FOR UPDATE`).Scan(&c.ID, &c.Tokens)
	if errors.Is(err, pgx.ErrNoRows) {
		return tx.Commit(ctx)
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE oura_connections SET tokens=NULL,token_expiry=NULL,status='disconnected',generation=generation+1,reconnect_reason=NULL WHERE id=$1`, c.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE jobs SET status='cancelled',completed_at=now() WHERE connection_id=$1 AND status IN ('queued','running')`, c.ID); err != nil {
		return err
	}
	if erase {
		for _, table := range []string{"oura_documents", "sync_state", "jobs", "webhook_inbox"} {
			if _, err = tx.Exec(ctx, "DELETE FROM "+table+" WHERE connection_id=$1", c.ID); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	// Oura documents revocation with an access_token query parameter. This client
	// never logs URLs; the token is not returned by the API or stored in jobs.
	if len(c.Tokens) > 0 {
		if plain, err := o.Store.Vault.Open(c.Tokens, "oura-tokens"); err == nil {
			var token oauth2.Token
			if json.Unmarshal(plain, &token) == nil {
				_ = o.request(ctx, http.MethodGet, "/oauth/revoke?access_token="+url.QueryEscape(token.AccessToken), "", nil, nil, false)
			}
		}
	}
	return nil
}
