package app

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

type Store struct {
	DB     *pgxpool.Pool
	Config Config
	Vault  *Vault
}

type Connection struct {
	ID         int64      `json:"-"`
	OuraUserID string     `json:"-"`
	Status     string     `json:"status"`
	Generation int64      `json:"-"`
	Tokens     []byte     `json:"-"`
	Expiry     *time.Time `json:"-"`
	Reason     *string    `json:"reconnect_reason,omitempty"`
}

type Coverage struct {
	Collection  string     `json:"collection"`
	LastSuccess *time.Time `json:"last_successful_sync_at"`
	Start       *time.Time `json:"coverage_start"`
	End         *time.Time `json:"coverage_end_exclusive"`
	Status      string     `json:"status"`
}

func Migrate(ctx context.Context, db *pgxpool.Pool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(42719021)"); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, schema); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Connection(ctx context.Context) (Connection, error) {
	var c Connection
	err := s.DB.QueryRow(ctx, `SELECT id,oura_user_id,status,generation,tokens,token_expiry,reconnect_reason
	 FROM oura_connections WHERE singleton`).
		Scan(&c.ID, &c.OuraUserID, &c.Status, &c.Generation, &c.Tokens, &c.Expiry, &c.Reason)
	return c, err
}

func (s *Store) Coverage(ctx context.Context, connection int64) ([]Coverage, error) {
	rows, err := s.DB.Query(ctx, `SELECT collection,last_success_at,coverage_start,coverage_end,status
	 FROM sync_state WHERE connection_id=$1 ORDER BY collection`, connection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Coverage{}
	for rows.Next() {
		var c Coverage
		if err := rows.Scan(&c.Collection, &c.LastSuccess, &c.Start, &c.End, &c.Status); err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

type Document struct {
	Collection string          `json:"collection"`
	ID         string          `json:"id"`
	Day        *time.Time      `json:"day"`
	Payload    json.RawMessage `json:"data"`
	FetchedAt  time.Time       `json:"fetched_at"`
}

func (s *Store) Documents(ctx context.Context, conn int64, collections []string, start, end time.Time, cursor string) ([]Document, error) {
	rows, err := s.DB.Query(ctx, `SELECT collection,source_id,source_day,payload,fetched_at FROM oura_documents
	 WHERE connection_id=$1 AND collection=ANY($2) AND NOT deleted AND source_day >= $3 AND source_day < $4
	 AND (collection||':'||source_id)>$5 ORDER BY collection,source_id LIMIT 501`, conn, collections, start, end, cursor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docs := []Document{}
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.Collection, &d.ID, &d.Day, &d.Payload, &d.FetchedAt); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

type Job struct {
	ID, ConnectionID, Generation int64
	Kind                         string
	Args                         json.RawMessage
	Attempts                     int
}

func enqueue(ctx context.Context, tx pgx.Tx, c Connection, kind string, args any, key string) error {
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO jobs(connection_id,generation,kind,args,dedup_key) VALUES($1,$2,$3,$4,$5)
	 ON CONFLICT(dedup_key) DO NOTHING`, c.ID, c.Generation, kind, b, key)
	return err
}

func (s *Store) EnqueueRange(ctx context.Context, start, end time.Time) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var c Connection
	err = tx.QueryRow(ctx, `SELECT id,generation FROM oura_connections WHERE singleton
	 AND status='active' FOR UPDATE`).Scan(&c.ID, &c.Generation)
	if err != nil {
		return err
	}
	for at := start; at.Before(end); at = at.AddDate(0, 0, 7) {
		to := at.AddDate(0, 0, 7)
		if to.After(end) {
			to = end
		}
		for _, collection := range Collections {
			args := RangeJob{Collection: collection, Start: at.Format(time.DateOnly), End: to.Format(time.DateOnly)}
			if err = enqueue(ctx, tx, c, "range", args, fmt.Sprintf("range:%d:%s:%s:%s:%s", c.Generation, collection, args.Start, args.End, randomToken())); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) ReceiveWebhook(ctx context.Context, body []byte, event Webhook) error {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var c Connection
	err = tx.QueryRow(ctx, `SELECT id,generation FROM oura_connections WHERE oura_user_id=$1 AND status<>'disconnected'
	 AND singleton FOR UPDATE`, event.UserID).Scan(&c.ID, &c.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `INSERT INTO webhook_inbox(digest,connection_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, digest(string(body)), c.ID)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	if err = enqueue(ctx, tx, c, "webhook", event, fmt.Sprintf("webhook:%x", digest(string(body)))); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Claim(ctx context.Context) (Job, error) {
	var j Job
	err := s.DB.QueryRow(ctx, `WITH candidate AS (
	 SELECT id FROM jobs WHERE (status='queued' AND available_at<=now()) OR (status='running' AND leased_until<now())
	 ORDER BY available_at,id FOR UPDATE SKIP LOCKED LIMIT 1)
	 UPDATE jobs j SET status='running',attempts=attempts+1,leased_until=now()+interval '10 minutes'
	 FROM candidate WHERE j.id=candidate.id RETURNING j.id,j.connection_id,j.generation,j.kind,j.args,j.attempts`).
		Scan(&j.ID, &j.ConnectionID, &j.Generation, &j.Kind, &j.Args, &j.Attempts)
	return j, err
}

func (s *Store) Finish(ctx context.Context, j Job, code string, delay time.Duration) error {
	if code == "" {
		_, err := s.DB.Exec(ctx, `UPDATE jobs SET status='done',completed_at=now(),leased_until=NULL,last_error=NULL WHERE id=$1 AND status='running'`, j.ID)
		return err
	}
	status := "queued"
	if j.Attempts >= 10 {
		status = "failed"
	}
	_, err := s.DB.Exec(ctx, `UPDATE jobs SET status=$2,last_error=$3,available_at=now()+$4*interval '1 second',leased_until=NULL,
	 completed_at=CASE WHEN $2='failed' THEN now() ELSE NULL END WHERE id=$1 AND status='running'`, j.ID, status, code, delay.Seconds())
	return err
}
