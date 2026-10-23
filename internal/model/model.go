// Package model определяет данные и инварианты системы лояльности.
package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrInvalidInput обозначает неверный формат или недопустимую сумму.
	ErrInvalidInput = errors.New("invalid input")
	// ErrInvalidOrder обозначает номер, не прошедший проверку Луна.
	ErrInvalidOrder = errors.New("invalid order number")
	// ErrLoginTaken обозначает уже занятый логин.
	ErrLoginTaken = errors.New("login already taken")
	// ErrUnauthorized обозначает отсутствие подтверждённой учётной записи.
	ErrUnauthorized = errors.New("unauthorized")
	// ErrOrderOwned обозначает заказ, принятый от другого пользователя.
	ErrOrderOwned = errors.New("order belongs to another user")
	// ErrInsufficient обозначает недостаточный доступный баланс.
	ErrInsufficient = errors.New("insufficient balance")
)

const (
	// New обозначает заказ, ожидающий обработки.
	New = "NEW"
	// Processing обозначает расчёт начисления внешней системой.
	Processing = "PROCESSING"
	// Invalid обозначает окончательный отказ внешней системы.
	Invalid = "INVALID"
	// Processed обозначает окончательно полученный результат начисления.
	Processed = "PROCESSED"
)

// Money хранит неотрицательную сумму в сотых долях балла.
type Money int64

// MarshalJSON записывает точное число баллов без float64.
func (m Money) MarshalJSON() ([]byte, error) {
	if m < 0 {
		return nil, ErrInvalidInput
	}
	return []byte(strconv.FormatInt(int64(m)/100, 10) + "." + fmt.Sprintf("%02d", int64(m)%100)), nil
}

// UnmarshalJSON принимает точную неотрицательную сумму с точностью до сотых.
func (m *Money) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || len(data) > 128 || data[0] < '0' || data[0] > '9' || !json.Valid(data) {
		return ErrInvalidInput
	}
	if index := strings.IndexAny(string(data), "eE"); index >= 0 {
		exponent, err := strconv.ParseInt(string(data[index+1:]), 10, 32)
		if err != nil || exponent < -128 || exponent > 18 {
			return ErrInvalidInput
		}
	}
	r, ok := new(big.Rat).SetString(string(data))
	if !ok {
		return ErrInvalidInput
	}
	r.Mul(r, big.NewRat(100, 1))
	if !r.IsInt() || !r.Num().IsInt64() || r.Sign() < 0 {
		return ErrInvalidInput
	}
	*m = Money(r.Num().Int64())
	return nil
}

// ValidOrder проверяет строку цифр алгоритмом Луна без преобразования в int.
func ValidOrder(number string) bool {
	if number == "" {
		return false
	}
	sum := 0
	double := false
	for i := len(number) - 1; i >= 0; i-- {
		if number[i] < '0' || number[i] > '9' {
			return false
		}
		n := int(number[i] - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum = (sum + n) % 10
		double = !double
	}
	return sum == 0
}

// User содержит сохранённую учётную запись, включая хеш пароля.
type User struct {
	ID           int64
	Login        string
	PasswordHash string
}

// Order содержит номер, состояние и необязательное начисление.
type Order struct {
	Number     string    `json:"number"`
	Status     string    `json:"status"`
	Accrual    *Money    `json:"accrual,omitempty"`
	UploadedAt time.Time `json:"uploaded_at"`
}

// Balance содержит доступные и потраченные баллы.
type Balance struct {
	Current   Money `json:"current"`
	Withdrawn Money `json:"withdrawn"`
}

// Withdrawal описывает подтверждённое списание.
type Withdrawal struct {
	Order       string    `json:"order"`
	Sum         Money     `json:"sum"`
	ProcessedAt time.Time `json:"processed_at"`
}
