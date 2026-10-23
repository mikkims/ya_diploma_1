package config

import (
	"os"
	"testing"
)

func TestLoad(t *testing.T) {
	for _, tt := range []struct {
		name    string
		env     map[string]string
		args    []string
		want    Config
		wantErr bool
	}{
		{name: "defaults", args: []string{"-d", "postgres://localhost/test"}, want: Config{RunAddress: "localhost:8080", DatabaseURI: "postgres://localhost/test", AccrualAddress: "http://localhost:8081"}},
		{name: "environment", env: map[string]string{"RUN_ADDRESS": ":9999", "DATABASE_URI": "postgres://localhost/env", "ACCRUAL_SYSTEM_ADDRESS": "https://example.test", "AUTH_SECRET": "synthetic"}, want: Config{RunAddress: ":9999", DatabaseURI: "postgres://localhost/env", AccrualAddress: "https://example.test", AuthSecret: "synthetic"}},
		{name: "flags override environment", env: map[string]string{"RUN_ADDRESS": ":9999", "DATABASE_URI": "env", "ACCRUAL_SYSTEM_ADDRESS": "https://env.test"}, args: []string{"-a", ":8085", "-d", "flag", "-r", "file:///tmp/test"}, want: Config{RunAddress: ":8085", DatabaseURI: "flag", AccrualAddress: "file:///tmp/test"}},
		{name: "missing database", wantErr: true},
		{name: "empty address", env: map[string]string{"RUN_ADDRESS": ""}, args: []string{"-d", "test"}, wantErr: true},
		{name: "empty accrual", env: map[string]string{"ACCRUAL_SYSTEM_ADDRESS": ""}, args: []string{"-d", "test"}, wantErr: true},
		{name: "empty database", env: map[string]string{"DATABASE_URI": ""}, wantErr: true},
		{name: "flags override empty environment", env: map[string]string{"RUN_ADDRESS": "", "DATABASE_URI": "", "ACCRUAL_SYSTEM_ADDRESS": ""}, args: []string{"-a", ":8085", "-d", "test", "-r", "http://localhost:9000"}, want: Config{RunAddress: ":8085", DatabaseURI: "test", AccrualAddress: "http://localhost:9000"}},
		{name: "explicit empty flag", env: map[string]string{"DATABASE_URI": "test"}, args: []string{"-a", ""}, wantErr: true},
		{name: "unknown flag", args: []string{"-unknown"}, wantErr: true},
		{name: "positional argument", args: []string{"extra"}, wantErr: true},
		{name: "missing flag value", args: []string{"-d"}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{"RUN_ADDRESS", "DATABASE_URI", "ACCRUAL_SYSTEM_ADDRESS", "AUTH_SECRET"} {
				t.Setenv(key, "")
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			got, err := Load(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load error: %v", err)
			}
			if err == nil && got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
