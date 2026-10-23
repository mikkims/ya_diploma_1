package storage

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mikkims/ya_diploma_1/internal/model"
)

const testBalanceQuery = `
UPDATE users
SET balance = $2, withdrawn = $3
WHERE id = $1
`
const testRejectDebitQuery = `
ALTER TABLE users ADD CONSTRAINT test_debit CHECK (balance <> 500)
`
const testRejectHistoryQuery = `
ALTER TABLE withdrawals ADD CONSTRAINT test_history CHECK (order_number <> '00')
`
const testRejectCreditQuery = `
ALTER TABLE users ADD CONSTRAINT test_credit CHECK (balance <> 500)
`
const testRejectOrderQuery = `
ALTER TABLE orders ADD CONSTRAINT test_order CHECK (status <> 'PROCESSED')
`
const testRejectWithdrawalCommitQuery = `
CREATE FUNCTION reject_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'test commit failure'; END;
$$;
CREATE CONSTRAINT TRIGGER test_commit AFTER INSERT ON withdrawals
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_commit();
`
const testRejectAccrualCommitQuery = `
CREATE FUNCTION reject_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'test commit failure'; END;
$$;
CREATE CONSTRAINT TRIGGER test_commit AFTER UPDATE ON orders
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_commit();
`

func setTestBalance(t *testing.T, db *pgxpool.Pool, id int64, balance, withdrawn int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := db.Exec(ctx, testBalanceQuery, id, balance, withdrawn); err != nil {
		t.Fatal(err)
	}
}

func TestAccounts(t *testing.T) {
	p := New(temporaryPostgres(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := p.CreateUser(ctx, "alice", "hash")
	if err != nil || id < 1 {
		t.Fatalf("create: %d %v", id, err)
	}
	if _, err := p.CreateUser(ctx, "alice", "hash"); !errors.Is(err, model.ErrLoginTaken) {
		t.Fatal(err)
	}
	u, err := p.UserByLogin(ctx, "alice")
	if err != nil || u.ID != id || u.Login != "alice" || u.PasswordHash != "hash" {
		t.Fatalf("user: %+v %v", u, err)
	}
	if _, err := p.UserByLogin(ctx, "missing"); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err := p.Balance(ctx, id+100); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatal(err)
	}
}

func TestStorageClosedPool(t *testing.T) {
	db := temporaryPostgres(t)
	db.Close()
	p := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	checks := []func() error{
		func() error { _, err := p.CreateUser(ctx, "a", "h"); return err },
		func() error { _, err := p.UserByLogin(ctx, "a"); return err },
		func() error { _, err := p.AddOrder(ctx, 1, "00"); return err },
		func() error { _, err := p.Orders(ctx, 1); return err },
		func() error { _, err := p.Balance(ctx, 1); return err },
		func() error { _, err := p.Withdrawals(ctx, 1); return err },
		func() error { _, err := p.Pending(ctx); return err },
		func() error { return p.Withdraw(ctx, 1, "00", 100) },
		func() error { return p.Apply(ctx, "00", model.Processed, nil) },
	}
	for i, check := range checks {
		if err := check(); err == nil {
			t.Errorf("operation %d hid database error", i)
		}
	}
}

func TestWithdrawalAtomicity(t *testing.T) {
	for _, phase := range []string{"ok", "insufficient", "overflow", "lock", "debit", "history", "commit"} {
		t.Run(phase, func(t *testing.T) {
			db := temporaryPostgres(t)
			p := New(db)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			id, err := p.CreateUser(ctx, "alice", "hash")
			if err != nil {
				t.Fatal(err)
			}
			balance, withdrawn := int64(1000), int64(0)
			if phase == "insufficient" {
				balance = 0
			}
			if phase == "overflow" {
				withdrawn = math.MaxInt64
			}
			setTestBalance(t, db, id, balance, withdrawn)
			query := map[string]string{"debit": testRejectDebitQuery, "history": testRejectHistoryQuery, "commit": testRejectWithdrawalCommitQuery}[phase]
			if query != "" {
				if _, err := db.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			target := id
			if phase == "lock" {
				target += 100
			}
			err = p.Withdraw(ctx, target, "00", 500)
			if (err == nil) != (phase == "ok") {
				t.Fatalf("withdraw: %v", err)
			}
			if phase == "insufficient" && !errors.Is(err, model.ErrInsufficient) {
				t.Fatal(err)
			}
			want := model.Balance{Current: model.Money(balance), Withdrawn: model.Money(withdrawn)}
			wantHistory := 0
			if phase == "ok" {
				want.Current -= 500
				want.Withdrawn += 500
				wantHistory = 1
			}
			got, err := p.Balance(ctx, id)
			if err != nil || got != want {
				t.Fatalf("balance %+v, want %+v: %v", got, want, err)
			}
			history, err := p.Withdrawals(ctx, id)
			if err != nil || len(history) != wantHistory {
				t.Fatalf("history: %v, %v", history, err)
			}
		})
	}
}

func TestAccrualAtomicity(t *testing.T) {
	for _, phase := range []string{"ok", "terminal", "zero", "missing", "pending", "lock", "credit", "overflow", "update", "commit"} {
		t.Run(phase, func(t *testing.T) {
			db := temporaryPostgres(t)
			p := New(db)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			id, err := p.CreateUser(ctx, "alice", "hash")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.AddOrder(ctx, id, "00"); err != nil {
				t.Fatal(err)
			}
			amount := model.Money(500)
			value := &amount
			status := model.Processed
			current := model.New
			wantBalance := model.Money(0)
			switch phase {
			case "terminal":
				if err := p.Apply(ctx, "00", model.Invalid, nil); err != nil {
					t.Fatal(err)
				}
				current = model.Invalid
			case "zero":
				amount = 0
			case "missing":
				value = nil
			case "pending":
				status = ""
				value = nil
			case "overflow":
				setTestBalance(t, db, id, math.MaxInt64, 0)
				wantBalance = model.Money(math.MaxInt64)
			}
			query := map[string]string{"credit": testRejectCreditQuery, "update": testRejectOrderQuery, "commit": testRejectAccrualCommitQuery}[phase]
			if query != "" {
				if _, err := db.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			number := "00"
			if phase == "lock" {
				number = "18"
			}
			err = p.Apply(ctx, number, status, value)
			success := phase == "ok" || phase == "terminal" || phase == "zero" || phase == "missing" || phase == "pending"
			if (err == nil) != success {
				t.Fatalf("apply: %v", err)
			}
			if success && phase != "terminal" && phase != "pending" {
				current = model.Processed
				if value != nil {
					wantBalance = *value
				}
			}
			balance, err := p.Balance(ctx, id)
			if err != nil || balance.Current != wantBalance {
				t.Fatalf("balance: %+v, want %d: %v", balance, wantBalance, err)
			}
			orders, err := p.Orders(ctx, id)
			if err != nil || len(orders) != 1 || orders[0].Status != current {
				t.Fatalf("orders: %+v, %v", orders, err)
			}
			wantAmount := value
			if !success || phase == "terminal" {
				wantAmount = nil
			}
			gotAmount := orders[0].Accrual
			if (gotAmount == nil) != (wantAmount == nil) || (wantAmount != nil && *gotAmount != *wantAmount) {
				t.Fatal("nullable amount changed")
			}
		})
	}
}

func TestCanceledTransactionReleasesPool(t *testing.T) {
	db := temporaryPostgres(t)
	p := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := p.CreateUser(ctx, "alice", "hash")
	if err != nil {
		t.Fatal(err)
	}
	setTestBalance(t, db, id, 1000, 0)
	blocker, err := db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }()
	if _, err := blocker.Exec(ctx, withdrawBalanceQuery, id); err != nil {
		t.Fatal(err)
	}
	blockedCtx, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	err = p.Withdraw(blockedCtx, id, "00", 100)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked withdrawal: %v", err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Withdraw(ctx, id, "00", 100); err != nil {
		t.Fatalf("pool unusable after cancellation: %v", err)
	}
	balance, err := p.Balance(ctx, id)
	if err != nil || balance.Current != 900 || balance.Withdrawn != 100 {
		t.Fatalf("balance: %+v %v", balance, err)
	}
	if db.Stat().AcquiredConns() != 0 {
		t.Fatal("transaction retained a pool connection")
	}
}

func TestStorageInputValidation(t *testing.T) {
	p := New(nil)
	negative := model.Money(-1)
	for _, err := range []error{
		p.Withdraw(context.Background(), 1, "00", 0),
		p.Apply(context.Background(), "00", "UNKNOWN", nil),
		p.Apply(context.Background(), "00", model.Processed, &negative),
		p.Apply(context.Background(), "00", model.Processing, moneyForStorage(1)),
	} {
		if !errors.Is(err, model.ErrInvalidInput) {
			t.Fatal(err)
		}
	}
}

func moneyForStorage(v model.Money) *model.Money { return &v }

func TestStringColumnLimits(t *testing.T) {
	db := temporaryPostgres(t)
	p := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := p.CreateUser(ctx, strings.Repeat("a", 256), strings.Repeat("h", 60))
	if err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if _, err := p.CreateUser(ctx, strings.Repeat("b", 257), "h"); !errors.As(err, &pgErr) || pgErr.Code != "22001" {
		t.Fatalf("login limit: %v", err)
	}
	if _, err := p.CreateUser(ctx, "hash-too-long", strings.Repeat("h", 61)); !errors.As(err, &pgErr) || pgErr.Code != "22001" {
		t.Fatalf("hash limit: %v", err)
	}
	if _, err := p.AddOrder(ctx, id, strings.Repeat("0", 16385)); !errors.As(err, &pgErr) || pgErr.Code != "22001" {
		t.Fatalf("number limit: %v", err)
	}
	setTestBalance(t, db, id, 100, 0)
	if err := p.Withdraw(ctx, id, strings.Repeat("0", 16384), 1); err != nil {
		t.Fatal(err)
	}
	if err := p.Withdraw(ctx, id, strings.Repeat("0", 16385), 1); !errors.As(err, &pgErr) || pgErr.Code != "22001" {
		t.Fatalf("withdrawal number limit: %v", err)
	}
	b, err := p.Balance(ctx, id)
	if err != nil || b.Current != 99 || b.Withdrawn != 1 {
		t.Fatalf("length failure did not roll back: %+v %v", b, err)
	}
}
