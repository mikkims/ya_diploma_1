package config

import "testing"

func TestLoad(t *testing.T) {
	for _, key := range []string{"RUN_ADDRESS", "DATABASE_URI", "ACCRUAL_SYSTEM_ADDRESS", "AUTH_SECRET"} {
		t.Setenv(key, "")
	}
	if _, err := Load(nil); err == nil {
		t.Fatal("missing DB accepted")
	}
	c, err := Load([]string{"-d", "postgres://localhost/test", "-a", ":8085", "-r", "http://localhost:9000"})
	if err != nil || c.RunAddress != ":8085" || c.AccrualAddress != "http://localhost:9000" {
		t.Fatalf("flags: %+v %v", c, err)
	}
	t.Setenv("RUN_ADDRESS", ":9999")
	t.Setenv("DATABASE_URI", "postgres://localhost/env")
	t.Setenv("ACCRUAL_SYSTEM_ADDRESS", "https://example.test")
	t.Setenv("AUTH_SECRET", "synthetic")
	c, err = Load([]string{"-a", ":8080", "-d", "ignored"})
	if err != nil || c.RunAddress != ":9999" || c.DatabaseURI != "postgres://localhost/env" || c.AccrualAddress != "https://example.test" || c.AuthSecret != "synthetic" {
		t.Fatal("env priority")
	}
	for _, args := range [][]string{{"-unknown"}, {"extra"}, {"-d"}} {
		if _, err := Load(args); err == nil {
			t.Fatalf("accepted args %v", args)
		}
	}
	t.Setenv("ACCRUAL_SYSTEM_ADDRESS", "")
	c, err = Load([]string{"-r", "file:///tmp/test"})
	if err != nil || c.AccrualAddress != "file:///tmp/test" {
		t.Fatal("config must only load the URL")
	}
}
