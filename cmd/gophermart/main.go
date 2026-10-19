package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mikkims/ya_diploma_1/internal/accrual"
	"github.com/mikkims/ya_diploma_1/internal/auth"
	"github.com/mikkims/ya_diploma_1/internal/config"
	"github.com/mikkims/ya_diploma_1/internal/database"
	"github.com/mikkims/ya_diploma_1/internal/handler"
	"github.com/mikkims/ya_diploma_1/internal/logger"
	"github.com/mikkims/ya_diploma_1/internal/service"
	"github.com/mikkims/ya_diploma_1/internal/storage"
	"github.com/mikkims/ya_diploma_1/migrations"
	"github.com/rs/zerolog"
)

func main() {
	log := zerolog.New(os.Stderr).With().Timestamp().Logger()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], log); err != nil {
		logger.Failure(log.Error(), "gophermart.run", err).Msg("gophermart stopped with an error")
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, log zerolog.Logger) error {
	cfg, err := config.Load(args)
	if err != nil {
		logger.Failure(log.Error(), "config.load", err).Msg("startup failed")
		return err
	}
	a, err := auth.New(cfg.AuthSecret)
	if err != nil {
		logger.Failure(log.Error(), "auth.new", err).Msg("startup failed")
		return err
	}
	client, err := accrual.New(cfg.AccrualAddress)
	if err != nil {
		logger.Failure(log.Error(), "accrual.new", err).Msg("startup failed")
		return err
	}
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	db, err := database.Open(openCtx, cfg.DatabaseURI)
	cancel()
	if err != nil {
		logger.Failure(log.Error(), "database.open", err).Msg("startup failed")
		return err
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Failure(log.Error(), "database.close", err).Msg("database close failed")
		}
	}()
	migrationCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	err = migrations.Up(migrationCtx, db)
	stop()
	if err != nil {
		logger.Failure(log.Error(), "migrations.up", err).Msg("startup failed")
		return err
	}
	store := storage.New(db)
	router := handler.NewRouter(service.New(store, store), a, log)
	server := &http.Server{
		Addr:              cfg.RunAddress,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16384}
	return serve(ctx, server, func(workerCtx context.Context) { accrual.Run(workerCtx, store, client, log) })
}

func serve(ctx context.Context, server *http.Server, worker func(context.Context)) error {
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return fmt.Errorf("listen HTTP: %w", err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	server.BaseContext = func(net.Listener) context.Context { return workerCtx }
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); worker(workerCtx) }()
	serverDone := make(chan struct{})
	var serveErr error
	go func() { defer close(serverDone); serveErr = server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case <-serverDone:
	}
	cancel()
	shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, server.Close())
	}
	<-serverDone
	<-workerDone
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr)
}
