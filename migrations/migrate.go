// Package migrations применяет встроенную начальную схему PostgreSQL.
package migrations

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed *.sql
var files embed.FS

// Up применяет миграции на выделенном соединении с ограничением SQL по времени.
func Up(ctx context.Context, pool *pgxpool.Pool) (err error) {
	config := pool.Config().ConnConfig.Copy()
	config.ConnectTimeout = 5 * time.Second
	config.RuntimeParams["statement_timeout"] = "5s"
	config.RuntimeParams["lock_timeout"] = "5s"
	db := stdlib.OpenDB(*config)
	db.SetMaxOpenConns(1)
	defer func() { err = errors.Join(err, db.Close()) }()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("migration connection: %w", err)
	}
	source, err := iofs.New(files, ".")
	if err != nil {
		return fmt.Errorf("migration source: %w", err)
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{StatementTimeout: 5 * time.Second})
	if err != nil {
		return errors.Join(fmt.Errorf("migration driver: %w", err), source.Close())
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		return errors.Join(fmt.Errorf("create migrator: %w", err), source.Close(), driver.Close())
	}
	defer func() {
		sourceErr, dbErr := m.Close()
		err = errors.Join(err, sourceErr, dbErr)
	}()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}
