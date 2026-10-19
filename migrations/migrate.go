// Package migrations применяет встроенную начальную схему PostgreSQL.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed *.sql
var files embed.FS

const migrationTimeoutsQuery = `
SET statement_timeout = '5s';
SET lock_timeout = '5s';
`

// Up применяет миграции на выделенном соединении с ограничением SQL по времени.
func Up(ctx context.Context, db *sql.DB) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration connection: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, sql.ErrConnDone) {
			err = errors.Join(err, fmt.Errorf("close migration connection: %w", closeErr))
		}
	}()
	if _, err := conn.ExecContext(ctx, migrationTimeoutsQuery); err != nil {
		return fmt.Errorf("migration timeouts: %w", err)
	}
	source, err := iofs.New(files, ".")
	if err != nil {
		return fmt.Errorf("migration source: %w", err)
	}
	driver, err := postgres.WithConnection(ctx, conn, &postgres.Config{StatementTimeout: 5 * time.Second})
	if err != nil {
		return errors.Join(fmt.Errorf("migration driver: %w", err), source.Close())
	}
	m, err := migrate.NewWithInstance("iofs", source, "postgres", driver)
	if err != nil {
		return errors.Join(fmt.Errorf("create migrator: %w", err), source.Close())
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
