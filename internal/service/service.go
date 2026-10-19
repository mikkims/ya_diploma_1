// Package service реализует пользовательские операции системы лояльности.
package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/mikkims/ya_diploma_1/internal/model"
	"golang.org/x/crypto/bcrypt"
)

// Users хранит учётные записи, используемые сервисом аутентификации.
type Users interface {
	CreateUser(context.Context, string, string) (int64, error)
	UserByLogin(context.Context, string) (model.User, error)
}

// Ledger хранит пользовательские заказы, баланс и списания.
type Ledger interface {
	AddOrder(context.Context, int64, string) (bool, error)
	Orders(context.Context, int64) ([]model.Order, error)
	Balance(context.Context, int64) (model.Balance, error)
	Withdraw(context.Context, int64, string, model.Money) error
	Withdrawals(context.Context, int64) ([]model.Withdrawal, error)
}

// Service проверяет входные данные и применяет операции к хранилищу.
type Service struct {
	users  Users
	ledger Ledger
}

// New связывает сервис с хранилищами учётных записей и баллов.
func New(users Users, ledger Ledger) *Service { return &Service{users: users, ledger: ledger} }

func validCredentials(login, password string) bool {
	return len(login) > 0 && len(login) <= 256 && strings.TrimSpace(login) != "" && len(password) > 0 && len(password) <= 72
}

// Register хеширует допустимый пароль и атомарно создаёт пользователя.
func (s *Service) Register(ctx context.Context, login, password string) (int64, error) {
	if !validCredentials(login, password) {
		return 0, model.ErrInvalidInput
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return 0, fmt.Errorf("hash password: %w", err)
	}
	return s.users.CreateUser(ctx, login, string(hash))
}

// Login проверяет пароль существующей учётной записи.
func (s *Service) Login(ctx context.Context, login, password string) (int64, error) {
	if !validCredentials(login, password) {
		return 0, model.ErrInvalidInput
	}
	u, err := s.users.UserByLogin(ctx, login)
	if err != nil {
		return 0, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return 0, model.ErrUnauthorized
	}
	return u.ID, nil
}

// AddOrder проверяет Лун и возвращает признак нового заказа.
func (s *Service) AddOrder(ctx context.Context, userID int64, number string) (bool, error) {
	if !model.ValidOrder(number) {
		return false, model.ErrInvalidOrder
	}
	return s.ledger.AddOrder(ctx, userID, number)
}

// Orders возвращает заказы текущего пользователя в порядке времени.
func (s *Service) Orders(ctx context.Context, userID int64) ([]model.Order, error) {
	return s.ledger.Orders(ctx, userID)
}

// Balance возвращает доступные и потраченные баллы текущего пользователя.
func (s *Service) Balance(ctx context.Context, userID int64) (model.Balance, error) {
	return s.ledger.Balance(ctx, userID)
}

// Withdraw проверяет номер и сумму перед транзакционным списанием.
func (s *Service) Withdraw(ctx context.Context, userID int64, number string, amount model.Money) error {
	if !model.ValidOrder(number) {
		return model.ErrInvalidOrder
	}
	if amount <= 0 {
		return model.ErrInvalidInput
	}
	return s.ledger.Withdraw(ctx, userID, number, amount)
}

// Withdrawals возвращает историю текущего пользователя.
func (s *Service) Withdrawals(ctx context.Context, userID int64) ([]model.Withdrawal, error) {
	return s.ledger.Withdrawals(ctx, userID)
}
