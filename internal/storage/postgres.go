// Package storage сохраняет пользователей и транзакционные операции PostgreSQL.
package storage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mikkims/ya_diploma_1/internal/model"
)

const createUserQuery = `
INSERT INTO users (login, password_hash)
VALUES ($1, $2)
RETURNING id
`
const userQuery = `
SELECT id, login, password_hash
FROM users
WHERE login = $1
`
const addOrderQuery = `
INSERT INTO orders (number, user_id)
VALUES ($1, $2)
ON CONFLICT ON CONSTRAINT orders_number_equal DO NOTHING
RETURNING number
`
const ownerQuery = `
SELECT user_id
FROM orders
WHERE number = $1
`
const ordersQuery = `
SELECT number, status, accrual, uploaded_at
FROM orders
WHERE user_id = $1
ORDER BY uploaded_at DESC, number DESC
`
const balanceQuery = `
SELECT balance, withdrawn
FROM users
WHERE id = $1
`
const withdrawBalanceQuery = `
SELECT balance, withdrawn
FROM users
WHERE id = $1
FOR UPDATE
`
const debitQuery = `
UPDATE users
SET balance = balance - $2, withdrawn = withdrawn + $2
WHERE id = $1
`
const withdrawalQuery = `
INSERT INTO withdrawals (user_id, order_number, amount)
VALUES ($1, $2, $3)
`
const withdrawalsQuery = `
SELECT order_number, amount, processed_at
FROM withdrawals
WHERE user_id = $1
ORDER BY processed_at DESC, id DESC
`
const pendingQuery = `
SELECT number
FROM orders
WHERE status IN ('NEW', 'PROCESSING')
ORDER BY updated_at, number
LIMIT 100
`
const lockOrderQuery = `
SELECT user_id, status
FROM orders
WHERE number = $1
FOR UPDATE
`
const creditQuery = `
UPDATE users
SET balance = balance + $2
WHERE id = $1 AND balance <= $3
`
const updateOrderQuery = `
UPDATE orders
SET status = CASE WHEN $2::text = '' THEN status ELSE $2::text::order_status END,
    accrual = $3,
    updated_at = clock_timestamp()
WHERE number = $1
`

// Postgres использует пул соединений; его закрывает владелец приложения.
type Postgres struct{ db *pgxpool.Pool }

// New создаёт хранилище на открытом пуле PostgreSQL.
func New(db *pgxpool.Pool) *Postgres { return &Postgres{db: db} }

// CreateUser атомарно создаёт уникальный логин и возвращает ID.
func (p *Postgres) CreateUser(ctx context.Context, login, hash string) (int64, error) {
	var id int64
	err := p.db.QueryRow(ctx, createUserQuery, login, hash).Scan(&id)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "users_login_key" {
		return 0, model.ErrLoginTaken
	}
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

// UserByLogin возвращает учётную запись без раскрытия отсутствия логина клиенту.
func (p *Postgres) UserByLogin(ctx context.Context, login string) (model.User, error) {
	var u model.User
	err := p.db.QueryRow(ctx, userQuery, login).Scan(&u.ID, &u.Login, &u.PasswordHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, model.ErrUnauthorized
	}
	if err != nil {
		return u, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

// AddOrder использует уникальность БД, затем различает свой и чужой повтор.
func (p *Postgres) AddOrder(ctx context.Context, userID int64, number string) (bool, error) {
	var inserted string
	err := p.db.QueryRow(ctx, addOrderQuery, number, userID).Scan(&inserted)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("insert order: %w", err)
	}
	var owner int64
	if err := p.db.QueryRow(ctx, ownerQuery, number).Scan(&owner); err != nil {
		return false, fmt.Errorf("get order owner: %w", err)
	}
	if owner != userID {
		return false, model.ErrOrderOwned
	}
	return false, nil
}

// Orders возвращает только заказы пользователя от новых к старым.
func (p *Postgres) Orders(ctx context.Context, userID int64) (result []model.Order, err error) {
	rows, err := p.db.Query(ctx, ordersQuery, userID)
	if err != nil {
		return nil, fmt.Errorf("list orders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var o model.Order
		var amount *int64
		if err := rows.Scan(&o.Number, &o.Status, &amount, &o.UploadedAt); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		if amount != nil {
			value := model.Money(*amount)
			o.Accrual = &value
		}
		result = append(result, o)
	}
	return result, rows.Err()
}

// Balance возвращает сохранённые суммы одного пользователя.
func (p *Postgres) Balance(ctx context.Context, userID int64) (model.Balance, error) {
	var b model.Balance
	err := p.db.QueryRow(ctx, balanceQuery, userID).Scan(&b.Current, &b.Withdrawn)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, model.ErrUnauthorized
	}
	if err != nil {
		return b, fmt.Errorf("get balance: %w", err)
	}
	return b, nil
}

// Withdraw блокирует баланс и записывает историю в одной транзакции.
func (p *Postgres) Withdraw(ctx context.Context, userID int64, number string, amount model.Money) (err error) {
	if amount <= 0 {
		return model.ErrInvalidInput
	}
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin withdrawal: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rollback withdrawal: %w", rollbackErr))
		}
	}()
	var balance, withdrawn model.Money
	if err := tx.QueryRow(ctx, withdrawBalanceQuery, userID).Scan(&balance, &withdrawn); err != nil {
		return fmt.Errorf("lock balance: %w", err)
	}
	if balance < amount {
		return model.ErrInsufficient
	}
	if withdrawn > model.Money(math.MaxInt64)-amount {
		return fmt.Errorf("withdrawn total overflow")
	}
	if _, err := tx.Exec(ctx, debitQuery, userID, amount); err != nil {
		return fmt.Errorf("debit balance: %w", err)
	}
	if _, err := tx.Exec(ctx, withdrawalQuery, userID, number, amount); err != nil {
		return fmt.Errorf("record withdrawal: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit withdrawal: %w", err)
	}
	return nil
}

// Withdrawals возвращает историю только указанного пользователя.
func (p *Postgres) Withdrawals(ctx context.Context, userID int64) (result []model.Withdrawal, err error) {
	rows, err := p.db.Query(ctx, withdrawalsQuery, userID)
	if err != nil {
		return nil, fmt.Errorf("list withdrawals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var w model.Withdrawal
		if err := rows.Scan(&w.Order, &w.Sum, &w.ProcessedAt); err != nil {
			return nil, fmt.Errorf("scan withdrawal: %w", err)
		}
		result = append(result, w)
	}
	return result, rows.Err()
}

// Pending возвращает ограниченную порцию наиболее давно опрошенных заказов.
func (p *Postgres) Pending(ctx context.Context) (result []string, err error) {
	rows, err := p.db.Query(ctx, pendingQuery)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var number string
		if err := rows.Scan(&number); err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		result = append(result, number)
	}
	return result, rows.Err()
}

// Apply сохраняет статус и начисление атомарно; окончательный заказ не меняется.
// Пустой статус обновляет время опроса, сохраняя NEW/PROCESSING после HTTP 204.
func (p *Postgres) Apply(ctx context.Context, number, status string, amount *model.Money) (err error) {
	if status != "" && status != model.Processing && status != model.Invalid && status != model.Processed {
		return model.ErrInvalidInput
	}
	if amount != nil && (*amount < 0 || status != model.Processed) {
		return model.ErrInvalidInput
	}
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin accrual: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rollback accrual: %w", rollbackErr))
		}
	}()
	var userID int64
	var current string
	if err := tx.QueryRow(ctx, lockOrderQuery, number).Scan(&userID, &current); err != nil {
		return fmt.Errorf("lock order: %w", err)
	}
	if current == model.Invalid || current == model.Processed {
		return nil
	}
	if amount != nil && *amount > 0 {
		result, err := tx.Exec(ctx, creditQuery, userID, *amount, model.Money(math.MaxInt64)-*amount)
		if err != nil {
			return fmt.Errorf("credit balance: %w", err)
		}
		count := result.RowsAffected()
		if count != 1 {
			return fmt.Errorf("balance overflow or missing user")
		}
	}
	if _, err := tx.Exec(ctx, updateOrderQuery, number, status, amount); err != nil {
		return fmt.Errorf("update order: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit accrual: %w", err)
	}
	return nil
}
