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

// Load разбирает аргументы; непустые переменные окружения имеют приоритет.
func Load(args []string) (Config, error) {
	c := Config{}
	fs := flag.NewFlagSet("gophermart", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.RunAddress, "a", "localhost:8080", "HTTP address")
	fs.StringVar(&c.DatabaseURI, "d", "", "PostgreSQL URI")
	fs.StringVar(&c.AccrualAddress, "r", "http://localhost:8081", "accrual URL")
	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parse flags: %w", err)
	}
	if fs.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments")
	}
	for key, dest := range map[string]*string{"RUN_ADDRESS": &c.RunAddress, "DATABASE_URI": &c.DatabaseURI, "ACCRUAL_SYSTEM_ADDRESS": &c.AccrualAddress} {
		if value := os.Getenv(key); value != "" {
			*dest = value
		}
	}
	c.AuthSecret = os.Getenv("AUTH_SECRET")
	if c.DatabaseURI == "" || c.RunAddress == "" || c.AccrualAddress == "" {
		return Config{}, fmt.Errorf("invalid or missing configuration")
	}
	return c, nil
}
