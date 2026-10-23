// Package database открывает ограниченный пул соединений PostgreSQL.
package database

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Open подключается к PostgreSQL и проверяет соединение в пределах context.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	dsn, err := withTLSDefault(dsn)
	if err != nil {
		return nil, fmt.Errorf("database TLS configuration: %w", err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = 30 * time.Minute
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	if err := db.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

// withTLSDefault принимает PostgreSQL URI и сохраняет прежний require по умолчанию.
func withTLSDefault(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil || strings.ContainsAny(dsn, " \t\r\n\v\f") || u.Opaque != "" || u.Fragment != "" || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", fmt.Errorf("database address must be a PostgreSQL URI")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid database URI parameters")
	}
	if query.Has("service") || query.Has("servicefile") || query.Has("ssl") || os.Getenv("PGSERVICE") != "" {
		return "", fmt.Errorf("database service configuration and ssl alias are not supported")
	}
	if modes, present := query["sslmode"]; present {
		if len(modes) != 1 || modes[0] == "" {
			return "", fmt.Errorf("sslmode must occur once and must not be empty")
		}
		return dsn, nil
	}
	mode := os.Getenv("PGSSLMODE")
	if mode == "" {
		mode = "require"
	}
	if u.RawQuery != "" && !strings.HasSuffix(u.RawQuery, "&") {
		u.RawQuery += "&"
	}
	u.RawQuery += "sslmode=" + url.PathEscape(mode)
	return u.String(), nil
}
