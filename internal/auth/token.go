// Package auth подписывает и проверяет пользовательские JWT.
package auth

import (
	"crypto/rand"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Auth владеет ключом подписи одного экземпляра приложения.
type Auth struct{ key []byte }

// New использует секрет длиной не менее 32 байт либо случайный ключ процесса.
func New(secret string) (*Auth, error) {
	key := []byte(secret)
	if secret == "" {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate signing key: %w", err)
		}
	} else if len(key) < 32 {
		return nil, fmt.Errorf("AUTH_SECRET must contain at least 32 bytes")
	}
	return &Auth{key: key}, nil
}

// Token выпускает JWT для сохранённого пользователя на 24 часа.
func (a *Auth) Token(userID int64) (string, error) {
	if userID <= 0 {
		return "", fmt.Errorf("invalid user ID")
	}
	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   strconv.FormatInt(userID, 10),
		Issuer:    "gophermart",
		Audience:  jwt.ClaimStrings{"gophermart"},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(24 * time.Hour))}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(a.key)
}

// UserID проверяет подпись, срок, issuer, audience и положительный subject.
func (a *Auth) UserID(value string) (int64, bool) {
	claims := new(jwt.RegisteredClaims)
	token, err := jwt.ParseWithClaims(value, claims, func(t *jwt.Token) (any, error) { return a.key, nil },
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuer("gophermart"),
		jwt.WithAudience("gophermart"),
		jwt.WithIssuedAt())
	if err != nil || !token.Valid {
		return 0, false
	}
	id, err := strconv.ParseInt(claims.Subject, 10, 64)
	return id, err == nil && id > 0
}
