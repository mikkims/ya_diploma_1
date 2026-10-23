package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestTokens(t *testing.T) {
	a, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	token, err := a.Token(42)
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := a.UserID(token); !ok || id != 42 {
		t.Fatal("token rejected")
	}
	if _, ok := b.UserID(token); ok {
		t.Fatal("process keys match")
	}
	if _, ok := a.UserID(token + "x"); ok {
		t.Fatal("tampered accepted")
	}
	if _, err := a.Token(0); err == nil {
		t.Fatal("zero ID")
	}
	if _, err := New("short"); err == nil {
		t.Fatal("short key")
	}
	a, _ = New(strings.Repeat("a", 32))
	for _, tt := range []struct {
		name   string
		method jwt.SigningMethod
		claims jwt.RegisteredClaims
	}{
		{"expired", jwt.SigningMethodHS256, jwt.RegisteredClaims{Subject: "1", Issuer: "gophermart", Audience: jwt.ClaimStrings{"gophermart"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour))}},
		{"no-expiry", jwt.SigningMethodHS256, jwt.RegisteredClaims{Subject: "1", Issuer: "gophermart", Audience: jwt.ClaimStrings{"gophermart"}}},
		{"no-subject", jwt.SigningMethodHS256, jwt.RegisteredClaims{Issuer: "gophermart", Audience: jwt.ClaimStrings{"gophermart"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}},
		{"wrong-method", jwt.SigningMethodHS384, jwt.RegisteredClaims{Subject: "1", Issuer: "gophermart", Audience: jwt.ClaimStrings{"gophermart"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}},
		{"wrong-issuer", jwt.SigningMethodHS256, jwt.RegisteredClaims{Subject: "1", Issuer: "other", Audience: jwt.ClaimStrings{"gophermart"}, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			value, err := jwt.NewWithClaims(tt.method, tt.claims).SignedString(a.key)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := a.UserID(value); ok {
				t.Fatal("invalid claims accepted")
			}
		})
	}
}
