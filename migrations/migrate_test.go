package migrations

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

const createMigrationTestDatabaseQuery = `
CREATE DATABASE %s
`
const dropMigrationTestDatabaseQuery = `
DROP DATABASE %s
`
const identityColumnsQuery = `
SELECT count(*)
FROM information_schema.columns
WHERE table_schema = current_schema()
  AND table_name IN ('users', 'orders', 'withdrawals')
  AND column_name = 'id'
  AND is_identity = 'YES'
  AND identity_generation = 'ALWAYS'
`
const primaryKeysQuery = `
SELECT count(*)
FROM information_schema.table_constraints
WHERE table_schema = current_schema()
  AND table_name IN ('users', 'orders', 'withdrawals')
  AND constraint_type = 'PRIMARY KEY'
`
const stringColumnsQuery = `
SELECT table_name || '.' || column_name, character_maximum_length
FROM information_schema.columns
WHERE table_schema = current_schema() AND data_type = 'character varying'
`
const enumValuesQuery = `
SELECT enumlabel
FROM pg_enum
JOIN pg_type ON pg_type.oid = enumtypid
WHERE typname = 'order_status'
ORDER BY enumsortorder
`
const invalidEnumQuery = `
SELECT 'UNKNOWN'::order_status
`
const enumTypeCountQuery = `
SELECT count(*) FROM pg_type WHERE typname = 'order_status'
`
const migrationVersionQuery = `
SELECT version, dirty FROM schema_migrations
`

func migrationTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("GOPHERMART_TEST_DATABASE_URI")
	if dsn == "" {
		t.Skip("GOPHERMART_TEST_DATABASE_URI не задан: миграции PostgreSQL не проверены")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("Нужен URI отдельного тестового PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("gophermart_migrations_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, fmt.Sprintf(createMigrationTestDatabaseQuery, name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, fmt.Sprintf(dropMigrationTestDatabaseQuery, name)); err != nil {
			t.Error(err)
		}
	})
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestMigrationSchemaAndRollback(t *testing.T) {
	pool := migrationTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for range 2 {
		if err := Up(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("application pool closed by migrator: %v", err)
	}
	for _, query := range []string{identityColumnsQuery, primaryKeysQuery} {
		var count int
		if err := pool.QueryRow(ctx, query).Scan(&count); err != nil || count != 3 {
			t.Fatalf("identity/primary keys: %d %v", count, err)
		}
	}
	rows, err := pool.Query(ctx, stringColumnsQuery)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"users.login": 256, "users.password_hash": 60, "orders.number": 16384, "withdrawals.order_number": 16384}
	for rows.Next() {
		var name string
		var length int
		if err := rows.Scan(&name, &length); err != nil {
			t.Fatal(err)
		}
		if want[name] != length {
			t.Errorf("column %s length %d", name, length)
		}
		delete(want, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(want) != 0 {
		t.Fatalf("column limits: %v %v", want, err)
	}
	rows, err = pool.Query(ctx, enumValuesQuery)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	rows.Close()
	if err := rows.Err(); err != nil || strings.Join(values, ",") != "NEW,PROCESSING,INVALID,PROCESSED" {
		t.Fatalf("enum: %v %v", values, err)
	}
	var pgErr *pgconn.PgError
	if _, err := pool.Exec(ctx, invalidEnumQuery); !errors.As(err, &pgErr) || pgErr.Code != "22P02" {
		t.Fatalf("invalid status accepted: %v", err)
	}
	source, err := iofs.New(files, ".")
	if err != nil {
		t.Fatal(err)
	}
	bridge := stdlib.OpenDBFromPool(pool)
	driver, err := pgxmigrate.WithInstance(bridge, &pgxmigrate.Config{StatementTimeout: time.Second})
	if err != nil {
		_ = source.Close()
		_ = bridge.Close()
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "pgx5", driver)
	if err != nil {
		_ = source.Close()
		_ = driver.Close()
		t.Fatal(err)
	}
	downErr := m.Down()
	sourceErr, dbErr := m.Close()
	if err := errors.Join(downErr, sourceErr, dbErr); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, enumTypeCountQuery).Scan(&count); err != nil || count != 0 {
		t.Fatalf("down left ENUM: %d %v", count, err)
	}
	if err := Up(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var version int
	var dirty bool
	if err := pool.QueryRow(ctx, migrationVersionQuery).Scan(&version, &dirty); err != nil || version != 1 || dirty {
		t.Fatalf("reapplied schema: %d %v %v", version, dirty, err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := Up(canceled, pool); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled migration: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}
