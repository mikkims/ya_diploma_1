// Package config загружает параметры запуска из флагов и окружения.
package config

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// Config содержит параметры HTTP, PostgreSQL, начислений и подписи JWT.
type Config struct {
	RunAddress     string
	DatabaseURI    string
	AccrualAddress string
	AuthSecret     string
}

// Load разбирает окружение и флаги; явно переданные флаги имеют приоритет.
func Load(args []string) (Config, error) {
	c := Config{RunAddress: "localhost:8080", AccrualAddress: "http://localhost:8081"}
	for key, dest := range map[string]*string{"RUN_ADDRESS": &c.RunAddress, "DATABASE_URI": &c.DatabaseURI, "ACCRUAL_SYSTEM_ADDRESS": &c.AccrualAddress, "AUTH_SECRET": &c.AuthSecret} {
		if value, ok := os.LookupEnv(key); ok {
			*dest = value
		}
	}
	fs := flag.NewFlagSet("gophermart", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.RunAddress, "a", c.RunAddress, "HTTP address")
	fs.StringVar(&c.DatabaseURI, "d", c.DatabaseURI, "PostgreSQL URI")
	fs.StringVar(&c.AccrualAddress, "r", c.AccrualAddress, "accrual URL")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments")
	}
	if c.DatabaseURI == "" || c.RunAddress == "" || c.AccrualAddress == "" {
		return Config{}, fmt.Errorf("invalid or missing configuration")
	}
	return c, nil
}
