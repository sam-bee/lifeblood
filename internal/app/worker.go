package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

type RangeJob struct {
	Collection string `json:"collection"`
	Start      string `json:"start"`
	End        string `json:"end"`
}
type Webhook struct {
	Event      string    `json:"event_type"`
	Collection string    `json:"data_type"`
	ObjectID   string    `json:"object_id"`
	EventTime  time.Time `json:"event_time"`
	UserID     string    `json:"user_id"`
}

func (w Webhook) Valid() bool {
	return collectionAllowed(w.Collection) && slices.Contains([]string{"create", "update", "delete"}, w.Event) &&
		w.ObjectID != "" && len(w.ObjectID) <= 128 && w.UserID != "" && len(w.UserID) <= 128 && !w.EventTime.IsZero() && w.EventTime.Before(time.Now().Add(5*time.Minute))
}

type Worker struct {
	Store *Store
	Oura  *Oura
	Log   *slog.Logger
}

func (w *Worker) Run(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	maintenance := time.NewTicker(time.Hour)
	defer maintenance.Stop()
	w.maintain(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-maintenance.C:
			w.maintain(ctx)
		case <-tick.C:
			job, err := w.Store.Claim(ctx)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				w.Log.Error("queue claim failed")
				continue
			}
			jobCtx, cancel := context.WithTimeout(ctx, 8*time.Minute)
			err = w.Process(jobCtx, job)
			cancel()
			if ctx.Err() != nil {
				return
			} // lease expires and a new worker recovers it
			code := ""
			delay := time.Duration(0)
			if err != nil {
				code = "processing_failed"
				delay = time.Duration(min(1<<min(job.Attempts, 10), 3600))*time.Second + time.Duration(rand.IntN(10))*time.Second
				var upstream *UpstreamError
				if errors.As(err, &upstream) {
					code = upstream.Error()
					delay = max(delay, upstream.RetryAfter)
				}
				w.Log.Warn("ingestion job failed", "job_id", job.ID, "category", code)
			}
			if err = w.Store.Finish(ctx, job, code, delay); err != nil {
				w.Log.Error("queue completion failed", "job_id", job.ID)
			}
		}
	}
}

func (w *Worker) Process(ctx context.Context, j Job) error {
	c, err := w.Store.Connection(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if c.ID != j.ConnectionID || c.Generation != j.Generation || c.Status == "disconnected" {
		return nil
	}
	if c.Status != "active" {
		return errors.New("reconnect_required")
	}
	switch j.Kind {
	case "setup":
		if err = w.Oura.ReconcileSubscriptions(ctx); err != nil {
			return err
		}
		var r RangeJob
		if err = json.Unmarshal(j.Args, &r); err != nil {
			return err
		}
		start, e1 := time.Parse(time.DateOnly, r.Start)
		end, e2 := time.Parse(time.DateOnly, r.End)
		if e1 != nil || e2 != nil {
			return errors.New("invalid_range_job")
		}
		return w.Store.EnqueueRange(ctx, start, end)
	case "range":
		var r RangeJob
		if err = json.Unmarshal(j.Args, &r); err != nil {
			return err
		}
		return w.syncRange(ctx, c, r)
	case "webhook":
		var event Webhook
		if err = json.Unmarshal(j.Args, &event); err != nil {
			return err
		}
		return w.syncEvent(ctx, c, event)
	default:
		return errors.New("unknown_job_type")
	}
}

func decodeDocument(raw json.RawMessage) (string, time.Time, error) {
	var d struct{ ID, Day string }
	if err := json.Unmarshal(raw, &d); err != nil {
		return "", time.Time{}, err
	}
	day, err := time.Parse(time.DateOnly, d.Day)
	if d.ID == "" || len(d.ID) > 128 || err != nil {
		return "", time.Time{}, errors.New("invalid_oura_document")
	}
	return d.ID, day, nil
}

func (w *Worker) syncRange(ctx context.Context, c Connection, r RangeJob) error {
	if !collectionAllowed(r.Collection) {
		return errors.New("invalid_collection")
	}
	start, e1 := time.Parse(time.DateOnly, r.Start)
	end, e2 := time.Parse(time.DateOnly, r.End)
	if e1 != nil || e2 != nil || !end.After(start) || end.Sub(start) > 7*24*time.Hour {
		return errors.New("invalid_range_job")
	}
	var docs []json.RawMessage
	next := ""
	seen := map[string]bool{}
	for page := 0; ; page++ {
		if page >= 100 {
			return errors.New("pagination_limit")
		}
		q := url.Values{"start_date": {r.Start}, "end_date": {r.End}}
		if next != "" {
			q.Set("next_token", next)
		}
		var result struct {
			Data *[]json.RawMessage `json:"data"`
			Next string             `json:"next_token"`
		}
		err := w.Oura.Fetch(ctx, c, "/v2/usercollection/"+r.Collection+"?"+q.Encode(), &result)
		if err != nil {
			var upstream *UpstreamError
			if errors.As(err, &upstream) && upstream.Status == 403 {
				_, err = w.Store.DB.Exec(ctx, `INSERT INTO sync_state(connection_id,collection,status) SELECT id,$2,'access_unavailable' FROM oura_connections
				 WHERE id=$1 AND generation=$3 AND status='active' ON CONFLICT(connection_id,collection) DO UPDATE SET status='access_unavailable'`, c.ID, r.Collection, c.Generation)
				return err
			}
			return err
		}
		if result.Data == nil {
			return errors.New("oura_missing_data_array")
		}
		for _, raw := range *result.Data {
			_, day, err := decodeDocument(raw)
			if err != nil {
				return err
			}
			// Our end date is exclusive even if an upstream boundary is inclusive.
			if !day.Before(start) && day.Before(end) {
				docs = append(docs, raw)
			}
		}
		if len(docs) > 10000 {
			return errors.New("range_too_large")
		}
		if result.Next == "" {
			break
		}
		if seen[result.Next] {
			return errors.New("pagination_cycle")
		}
		seen[result.Next] = true
		next = result.Next
	}
	tx, err := w.Store.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockConnection(ctx, tx, c); err != nil {
		return err
	}
	ids := []string{}
	for _, raw := range docs {
		id, day, _ := decodeDocument(raw)
		ids = append(ids, id)
		if _, err = tx.Exec(ctx, `INSERT INTO oura_documents(connection_id,collection,source_id,source_day,payload)
		 VALUES($1,$2,$3,$4,$5) ON CONFLICT(connection_id,collection,source_id) DO UPDATE
		 SET source_day=EXCLUDED.source_day,payload=EXCLUDED.payload,deleted=false,fetched_at=now()`, c.ID, r.Collection, id, day, raw); err != nil {
			return err
		}
	}
	// A successfully completed authoritative window also catches missed deletes.
	if _, err = tx.Exec(ctx, `UPDATE oura_documents SET payload=NULL,deleted=true,fetched_at=now() WHERE connection_id=$1
	 AND collection=$2 AND source_day>=$3 AND source_day<$4 AND NOT(source_id=ANY($5))`, c.ID, r.Collection, start, end, ids); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO sync_state(connection_id,collection,last_success_at,coverage_start,coverage_end,status)
	 VALUES($1,$2,now(),$3,$4,'available') ON CONFLICT(connection_id,collection) DO UPDATE SET last_success_at=now(),
	 coverage_start=CASE WHEN sync_state.coverage_start IS NULL OR EXCLUDED.coverage_end<sync_state.coverage_start OR EXCLUDED.coverage_start>sync_state.coverage_end
	 THEN EXCLUDED.coverage_start ELSE LEAST(sync_state.coverage_start,EXCLUDED.coverage_start) END,
	 coverage_end=CASE WHEN sync_state.coverage_end IS NULL OR EXCLUDED.coverage_end<sync_state.coverage_start OR EXCLUDED.coverage_start>sync_state.coverage_end
	 THEN EXCLUDED.coverage_end ELSE GREATEST(sync_state.coverage_end,EXCLUDED.coverage_end) END,status='available'`, c.ID, r.Collection, start, end); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockConnection(ctx context.Context, tx pgx.Tx, c Connection) error {
	var id int64
	return tx.QueryRow(ctx, `SELECT id FROM oura_connections WHERE id=$1 AND generation=$2 AND status='active' FOR UPDATE`, c.ID, c.Generation).Scan(&id)
}

func (w *Worker) syncEvent(ctx context.Context, c Connection, e Webhook) error {
	if !e.Valid() || e.UserID != c.OuraUserID {
		return errors.New("invalid_webhook_job")
	}
	var previous *time.Time
	err := w.Store.DB.QueryRow(ctx, `SELECT event_at FROM oura_documents WHERE connection_id=$1 AND collection=$2 AND source_id=$3`, c.ID, e.Collection, e.ObjectID).Scan(&previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if previous != nil && e.EventTime.Before(*previous) {
		return nil
	}
	var raw json.RawMessage
	var day *time.Time
	deleted := e.Event == "delete"
	if !deleted {
		err = w.Oura.Fetch(ctx, c, "/v2/usercollection/"+e.Collection+"/"+url.PathEscape(e.ObjectID), &raw)
		var upstream *UpstreamError
		if errors.As(err, &upstream) && upstream.Status == 404 {
			deleted = true
			raw = nil
		} else if err != nil {
			return err
		}
		if !deleted {
			id, d, err := decodeDocument(raw)
			if err != nil {
				return err
			}
			if id != e.ObjectID {
				return errors.New("document_id_mismatch")
			}
			day = &d
		}
	}
	tx, err := w.Store.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = lockConnection(ctx, tx, c); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO oura_documents(connection_id,collection,source_id,source_day,payload,deleted,event_at)
	 VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(connection_id,collection,source_id) DO UPDATE SET
	 source_day=COALESCE(EXCLUDED.source_day,oura_documents.source_day),payload=EXCLUDED.payload,deleted=EXCLUDED.deleted,
	 event_at=EXCLUDED.event_at,fetched_at=now() WHERE oura_documents.event_at IS NULL OR oura_documents.event_at<EXCLUDED.event_at
	 OR (oura_documents.event_at=EXCLUDED.event_at AND EXCLUDED.deleted)`,
		c.ID, e.Collection, e.ObjectID, day, nullableJSON(raw), deleted, e.EventTime); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (w *Worker) maintain(ctx context.Context) {
	cleanup, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for _, query := range []string{`DELETE FROM oauth_flows WHERE expires_at<now()`, `DELETE FROM jobs WHERE completed_at<now()-interval '30 days'`,
		`DELETE FROM webhook_inbox WHERE received_at<now()-interval '30 days'`} {
		if _, err := w.Store.DB.Exec(cleanup, query); err != nil {
			w.Log.Error("maintenance cleanup failed")
			return
		}
	}
	if _, err := w.Store.DB.Exec(cleanup, `DELETE FROM oura_documents WHERE COALESCE(source_day::timestamptz,fetched_at)<now()-$1*interval '1 day'`, w.Store.Config.RetentionDays); err != nil {
		w.Log.Error("data retention cleanup failed")
	}
	c, err := w.Store.Connection(cleanup)
	if err != nil || c.Status != "active" || !w.Store.Config.OuraEnabled() {
		return
	}
	var pending bool
	if err = w.Store.DB.QueryRow(cleanup, `SELECT EXISTS(SELECT 1 FROM jobs WHERE connection_id=$1 AND kind='setup' AND status IN ('queued','running'))`, c.ID).Scan(&pending); err != nil || pending {
		return
	}
	if err = w.Oura.ReconcileSubscriptions(cleanup); err != nil {
		w.Log.Warn("webhook subscription reconciliation failed")
		return
	}
	var last *time.Time
	if err = w.Store.DB.QueryRow(cleanup, `SELECT min(last_success_at) FROM sync_state WHERE connection_id=$1`, c.ID).Scan(&last); err != nil {
		return
	}
	if last != nil && last.After(time.Now().Add(-24*time.Hour)) {
		return
	}
	end := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	if err = w.Store.EnqueueRange(cleanup, end.AddDate(0, 0, -min(7, w.Store.Config.RetentionDays)), end); err != nil {
		w.Log.Warn("reconciliation enqueue failed")
	}
}
