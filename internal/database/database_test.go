package database

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOpenCanceled(t *testing.T) {
	t.Setenv("PGSERVICE", "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db, err := Open(ctx, "postgres://synthetic@127.0.0.1:1/test?sslmode=disable")
	if db != nil {
		db.Close()
		t.Fatal("canceled open returned a pool")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestDatabaseTLSConfiguration(t *testing.T) {
	const uri = "postgres://synthetic@localhost/test"
	cases := []struct {
		name, dsn, env, service, password string
		wantPlain, wantError, unsetEnv    bool
	}{
		{name: "default", dsn: uri, unsetEnv: true},
		{name: "empty environment", dsn: uri},
		{name: "postgresql scheme", dsn: "postgresql://synthetic@localhost/test"},
		{name: "empty query", dsn: uri + "?"},
		{name: "trailing separator", dsn: uri + "?application_name=test&"},
		{name: "URI overrides environment", dsn: uri + "?sslmode=require", env: "disable"},
		{name: "URI overrides invalid environment", dsn: uri + "?sslmode=require", env: "invalid"},
		{name: "encoded key", dsn: uri + "?%73slmode=require", env: "disable"},
		{name: "literal plus password", dsn: uri + "?password=synthetic+value", password: "synthetic+value"},
		{name: "encoded query password", dsn: uri + "?password=synthetic%3Bvalue%2B%26", password: "synthetic;value+&"},
		{name: "encoded userinfo password", dsn: "postgres://synthetic:synthetic%3F%40%2Bvalue@localhost/test", password: "synthetic?@+value"},
		{name: "query bytes", dsn: uri + "?application_name=synthetic%2Btest&password=synthetic+%26value", password: "synthetic+&value"},
		{name: "empty mode", dsn: uri + "?sslmode=", wantError: true},
		{name: "missing mode value", dsn: uri + "?sslmode", wantError: true},
		{name: "duplicate modes", dsn: uri + "?sslmode=prefer&sslmode=require", wantError: true},
		{name: "duplicate encoded key", dsn: uri + "?sslmode=require&%73slmode=disable", wantError: true},
		{name: "invalid mode", dsn: uri + "?sslmode=invalid", wantError: true},
		{name: "invalid environment", dsn: uri, env: "invalid", wantError: true},
		{name: "keyword DSN", dsn: "host=localhost user=synthetic", wantError: true},
		{name: "service keyword", dsn: "service=synthetic", wantError: true},
		{name: "service URI", dsn: uri + "?service=synthetic", wantError: true},
		{name: "servicefile URI", dsn: uri + "?servicefile=absent", wantError: true},
		{name: "environment service", dsn: uri, service: "synthetic", wantError: true},
		{name: "SSL alias", dsn: uri + "?ssl=true", wantError: true},
		{name: "encoded SSL alias", dsn: uri + "?%73sl=true", wantError: true},
		{name: "invalid escape", dsn: uri + "?sslmode=%", wantError: true},
		{name: "unescaped whitespace", dsn: uri + "? sslmode =", wantError: true},
		{name: "fragment", dsn: uri + "#sslmode=disable", wantError: true},
		{name: "other scheme", dsn: "http://localhost/test", wantError: true},
	}
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"} {
		plain := mode == "disable" || mode == "allow" || mode == "prefer"
		cases = append(cases, struct {
			name, dsn, env, service, password string
			wantPlain, wantError, unsetEnv    bool
		}{name: "URI " + mode, dsn: uri + "?sslmode=" + mode, env: "require", wantPlain: plain})
		cases = append(cases, struct {
			name, dsn, env, service, password string
			wantPlain, wantError, unsetEnv    bool
		}{name: "environment " + mode, dsn: uri, env: mode, wantPlain: plain})
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			for _, key := range []string{"PGSSLMODE", "PGSERVICE", "PGSSLCERT", "PGSSLKEY", "PGSSLROOTCERT", "PGSSLNEGOTIATION", "PGOPTIONS", "PGPASSWORD"} {
				t.Setenv(key, "")
			}
			t.Setenv("PGPASSFILE", filepath.Join(home, "absent-pass"))
			t.Setenv("PGSERVICEFILE", filepath.Join(home, "absent-service"))
			t.Setenv("PGSSLMODE", tt.env)
			t.Setenv("PGSERVICE", tt.service)
			if tt.unsetEnv {
				if err := os.Unsetenv("PGSSLMODE"); err != nil {
					t.Fatal(err)
				}
			}
			configured, err := withTLSDefault(tt.dsn)
			if err != nil {
				if tt.wantError {
					return
				}
				t.Fatal(err)
			}
			inputURI, err := url.Parse(tt.dsn)
			if err != nil {
				t.Fatal(err)
			}
			outputURI, err := url.Parse(configured)
			if err != nil {
				t.Fatal(err)
			}
			if _, present := inputURI.Query()["sslmode"]; !present {
				prefix := inputURI.RawQuery
				if prefix != "" && !strings.HasSuffix(prefix, "&") {
					prefix += "&"
				}
				if !strings.HasPrefix(outputURI.RawQuery, prefix+"sslmode=") {
					t.Fatal("unrelated query bytes changed")
				}
			} else if configured != tt.dsn {
				t.Fatal("explicit URI mode changed")
			}

			cfg, err := pgxpool.ParseConfig(configured)
			if tt.wantError {
				if err == nil {
					t.Fatal("invalid configuration accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			plain := cfg.ConnConfig.TLSConfig == nil
			for _, fallback := range cfg.ConnConfig.Fallbacks {
				plain = plain || fallback.TLSConfig == nil
			}
			if plain != tt.wantPlain {
				t.Fatalf("plaintext fallback=%v, want %v", plain, tt.wantPlain)
			}
			if cfg.ConnConfig.Password != tt.password {
				t.Fatal("unrelated password parameter changed")
			}
			if os.Getenv("PGSSLMODE") != tt.env {
				t.Fatal("process environment changed")
			}
		})
	}
}
