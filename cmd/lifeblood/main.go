package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sam-bee/lifeblood/internal/app"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, log); err != nil {
		log.Error("lifeblood stopped", "reason", err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger) error {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command != "serve" && command != "migrate" && command != "link" {
		return errors.New("usage: lifeblood [serve|migrate|link]")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid database configuration")
	}
	pc.MaxConns = 8
	pc.ConnConfig.RuntimeParams["timezone"] = "UTC"
	db, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return errors.New("database setup failed")
	}
	defer db.Close()
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err = db.Ping(startup); err != nil {
		return errors.New("database connection failed")
	}
	if command == "migrate" {
		if err = app.Migrate(startup, db); err != nil {
			return errors.New("database migration failed")
		}
		return nil
	}
	cfg, err := app.LoadConfig()
	if err != nil {
		return err
	}
	vault, err := app.NewVault(cfg.EncryptionKey)
	if err != nil {
		return errors.New("invalid encryption key")
	}
	store := &app.Store{DB: db, Config: cfg, Vault: vault}
	oura := app.NewOura(store)
	if command == "link" {
		link, err := oura.BeginLink(startup)
		if err != nil {
			return errors.New("Oura linking could not start; check configuration and migrations")
		}
		fmt.Fprintln(os.Stdout, "Open this Oura authorization link once within ten minutes:")
		fmt.Fprintln(os.Stdout, link)
		return nil
	}
	api := &app.Server{Store: store, Oura: oura, Log: log}
	servers := []*http.Server{
		{Addr: cfg.Address, Handler: api.PublicHandler()},
		{Addr: cfg.PrivateAddress, Handler: api.PrivateHandler()},
	}
	// Bind both listeners before starting ingestion or accepting any requests.
	listeners := make([]net.Listener, 0, len(servers))
	for _, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			return errors.New("HTTP listener could not bind")
		}
		defer listener.Close()
		listeners = append(listeners, listener)
		server.ReadHeaderTimeout = 5 * time.Second
		server.ReadTimeout = 15 * time.Second
		server.WriteTimeout = 30 * time.Second
		server.IdleTimeout = 60 * time.Second
		server.MaxHeaderBytes = 16 * 1024
	}
	worker := &app.Worker{Store: store, Oura: oura, Log: log}
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); worker.Run(workerCtx) }()
	serveErr := make(chan error, len(servers))
	for i, server := range servers {
		listener := listeners[i]
		go func() {
			log.Info("HTTP listener started", "address", server.Addr)
			serveErr <- server.Serve(listener)
		}()
	}
	var serveFailed bool
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		serveFailed = !errors.Is(err, http.ErrServerClosed)
	}
	stopWorker()
	shutdown, done := context.WithTimeout(context.Background(), 30*time.Second)
	defer done()
	var shutdownFailed bool
	for _, server := range servers {
		if server.Shutdown(shutdown) != nil {
			shutdownFailed = true
			_ = server.Close()
		}
	}
	select {
	case <-workerDone:
		if serveFailed {
			return errors.New("HTTP server failed")
		}
		if shutdownFailed {
			return errors.New("HTTP shutdown timed out")
		}
		return nil
	case <-shutdown.Done():
		return errors.New("worker shutdown timed out")
	}
}
