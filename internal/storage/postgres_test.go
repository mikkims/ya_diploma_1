package storage

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/mikkims/ya_diploma_1/internal/model"
)

func mockStore(t *testing.T) (*Postgres, sqlmock.Sqlmock) {
	t.Helper()
	db, m, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := m.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		m.ExpectClose()
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return New(db), m
}

func TestAccounts(t *testing.T) {
	p, m := mockStore(t)
	ctx := context.Background()
	m.ExpectQuery(createUserQuery).WithArgs("alice", "hash").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
	if id, err := p.CreateUser(ctx, "alice", "hash"); err != nil || id != 1 {
		t.Fatalf("create %d %v", id, err)
	}
	m.ExpectQuery(createUserQuery).WillReturnError(&pq.Error{Code: "23505", Constraint: "users_login_key"})
	if _, err := p.CreateUser(ctx, "alice", "hash"); !errors.Is(err, model.ErrLoginTaken) {
		t.Fatal(err)
	}
	m.ExpectQuery(createUserQuery).WillReturnError(errors.New("offline"))
	if _, err := p.CreateUser(ctx, "a", "h"); err == nil {
		t.Fatal("DB error hidden")
	}
	m.ExpectQuery(userQuery).WithArgs("alice").WillReturnRows(sqlmock.NewRows([]string{"id", "login", "password_hash"}).AddRow(1, "alice", "hash"))
	if u, err := p.UserByLogin(ctx, "alice"); err != nil || u.ID != 1 || u.PasswordHash != "hash" {
		t.Fatal("user")
	}
	m.ExpectQuery(userQuery).WillReturnError(sql.ErrNoRows)
	if _, err := p.UserByLogin(ctx, "missing"); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatal(err)
	}
	m.ExpectQuery(userQuery).WillReturnError(errors.New("offline"))
	if _, err := p.UserByLogin(ctx, "a"); err == nil {
		t.Fatal("DB error hidden")
	}
	m.ExpectQuery(balanceQuery).WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"balance", "withdrawn"}).AddRow(1050, 125))
	if b, err := p.Balance(ctx, 1); err != nil || b.Current != 1050 || b.Withdrawn != 125 {
		t.Fatal("balance")
	}
	m.ExpectQuery(balanceQuery).WillReturnError(sql.ErrNoRows)
	if _, err := p.Balance(ctx, 2); !errors.Is(err, model.ErrUnauthorized) {
		t.Fatal(err)
	}
	m.ExpectQuery(balanceQuery).WillReturnError(errors.New("offline"))
	if _, err := p.Balance(ctx, 1); err == nil {
		t.Fatal("DB error hidden")
	}
}

func TestOrderOwnership(t *testing.T) {
	for _, tt := range []struct {
		name     string
		inserted bool
		owner    int64
		want     error
	}{{"new", true, 1, nil}, {"own", false, 1, nil}, {"foreign", false, 2, model.ErrOrderOwned}} {
		t.Run(tt.name, func(t *testing.T) {
			p, m := mockStore(t)
			rows := sqlmock.NewRows([]string{"number"})
			if tt.inserted {
				rows.AddRow("00")
			}
			m.ExpectQuery(addOrderQuery).WithArgs("00", int64(1)).WillReturnRows(rows)
			if !tt.inserted {
				m.ExpectQuery(ownerQuery).WithArgs("00").WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(tt.owner))
			}
			created, err := p.AddOrder(context.Background(), 1, "00")
			if created != tt.inserted || !errors.Is(err, tt.want) {
				t.Fatalf("ownership %v %v", created, err)
			}
		})
	}
	p, m := mockStore(t)
	m.ExpectQuery(addOrderQuery).WillReturnError(errors.New("offline"))
	if _, err := p.AddOrder(context.Background(), 1, "0"); err == nil {
		t.Fatal("insert error")
	}
	m.ExpectQuery(addOrderQuery).WillReturnRows(sqlmock.NewRows([]string{"number"}))
	m.ExpectQuery(ownerQuery).WillReturnError(errors.New("offline"))
	if _, err := p.AddOrder(context.Background(), 1, "0"); err == nil {
		t.Fatal("owner error")
	}
}

func TestRollbackFailurePreservesCause(t *testing.T) {
	p, m := mockStore(t)
	failure := errors.New("rollback failed")
	m.ExpectBegin()
	m.ExpectQuery(withdrawBalanceQuery).WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"balance", "withdrawn"}).AddRow(0, 0))
	m.ExpectRollback().WillReturnError(failure)
	err := p.Withdraw(context.Background(), 1, "00", 100)
	if !errors.Is(err, model.ErrInsufficient) || !errors.Is(err, failure) {
		t.Fatalf("withdrawal causes lost: %v", err)
	}
	m.ExpectBegin()
	m.ExpectQuery(lockOrderQuery).WithArgs("00").WillReturnRows(sqlmock.NewRows([]string{"user_id", "status"}).AddRow(1, model.Processed))
	m.ExpectRollback().WillReturnError(failure)
	if err := p.Apply(context.Background(), "00", model.Processed, nil); !errors.Is(err, failure) {
		t.Fatalf("rollback failure hidden: %v", err)
	}
}

func TestLists(t *testing.T) {
	p, m := mockStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	m.ExpectQuery(ordersQuery).WithArgs(int64(3)).WillReturnRows(sqlmock.NewRows([]string{"number", "status", "accrual", "uploaded_at"}).AddRow("00", model.Processed, 0, now).AddRow("18", model.New, nil, now.Add(-time.Second)))
	items, err := p.Orders(ctx, 3)
	if err != nil || len(items) != 2 || items[0].Accrual == nil || *items[0].Accrual != 0 || items[1].Accrual != nil || !items[0].UploadedAt.After(items[1].UploadedAt) {
		t.Fatalf("orders %v %v", items, err)
	}
	m.ExpectQuery(withdrawalsQuery).WithArgs(int64(3)).WillReturnRows(sqlmock.NewRows([]string{"order_number", "amount", "processed_at"}).AddRow("00", 125, now))
	w, err := p.Withdrawals(ctx, 3)
	if err != nil || len(w) != 1 || w[0].Sum != 125 {
		t.Fatal("withdrawals")
	}
	m.ExpectQuery(pendingQuery).WillReturnRows(sqlmock.NewRows([]string{"number"}).AddRow("00").AddRow("18"))
	if pending, err := p.Pending(ctx); err != nil || len(pending) != 2 {
		t.Fatal("pending")
	}
	for _, kind := range []string{"orders", "withdrawals", "pending"} {
		t.Run(kind, func(t *testing.T) {
			p, m := mockStore(t)
			var err error
			switch kind {
			case "orders":
				m.ExpectQuery(ordersQuery).WillReturnError(errors.New("offline"))
				_, err = p.Orders(ctx, 1)
			case "withdrawals":
				m.ExpectQuery(withdrawalsQuery).WillReturnError(errors.New("offline"))
				_, err = p.Withdrawals(ctx, 1)
			case "pending":
				m.ExpectQuery(pendingQuery).WillReturnError(errors.New("offline"))
				_, err = p.Pending(ctx)
			}
			if err == nil {
				t.Fatal("query failure hidden")
			}
		})
	}
}

func TestWithdrawalAtomicity(t *testing.T) {
	for _, phase := range []string{"ok", "insufficient", "overflow", "begin", "lock", "debit", "history", "commit"} {
		t.Run(phase, func(t *testing.T) {
			p, m := mockStore(t)
			failure := errors.New("synthetic failure")
			if phase == "begin" {
				m.ExpectBegin().WillReturnError(failure)
			} else {
				m.ExpectBegin()
				balance, withdrawn := int64(1000), int64(0)
				if phase == "insufficient" {
					balance = 0
				}
				if phase == "overflow" {
					withdrawn = math.MaxInt64
				}
				if phase == "lock" {
					m.ExpectQuery(withdrawBalanceQuery).WithArgs(int64(1)).WillReturnError(failure)
				} else {
					m.ExpectQuery(withdrawBalanceQuery).WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows([]string{"balance", "withdrawn"}).AddRow(balance, withdrawn))
				}
				if phase != "lock" && phase != "insufficient" && phase != "overflow" {
					if phase == "debit" {
						m.ExpectExec(debitQuery).WithArgs(int64(1), int64(500)).WillReturnError(failure)
					} else {
						m.ExpectExec(debitQuery).WithArgs(int64(1), int64(500)).WillReturnResult(sqlmock.NewResult(0, 1))
						if phase == "history" {
							m.ExpectExec(withdrawalQuery).WithArgs(int64(1), "00", int64(500)).WillReturnError(failure)
						} else {
							m.ExpectExec(withdrawalQuery).WithArgs(int64(1), "00", int64(500)).WillReturnResult(sqlmock.NewResult(1, 1))
							if phase == "commit" {
								m.ExpectCommit().WillReturnError(failure)
							} else {
								m.ExpectCommit()
							}
						}
					}
				}
				if phase != "ok" && phase != "commit" {
					m.ExpectRollback()
				}
			}
			err := p.Withdraw(context.Background(), 1, "00", 500)
			if (err == nil) != (phase == "ok") {
				t.Fatalf("withdrawal %v", err)
			}
			if phase == "insufficient" && !errors.Is(err, model.ErrInsufficient) {
				t.Fatal(err)
			}
		})
	}
	p, _ := mockStore(t)
	if err := p.Withdraw(context.Background(), 1, "0", 0); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatal(err)
	}
}

func TestAccrualAtomicity(t *testing.T) {
	for _, phase := range []string{"ok", "terminal", "zero", "missing", "pending", "begin", "lock", "credit", "overflow", "update", "commit"} {
		t.Run(phase, func(t *testing.T) {
			p, m := mockStore(t)
			failure := errors.New("synthetic failure")
			amount := model.Money(500)
			value := &amount
			status := model.Processed
			if phase == "zero" {
				amount = 0
			}
			if phase == "missing" {
				value = nil
			}
			if phase == "pending" {
				status = ""
				value = nil
			}
			if phase == "begin" {
				m.ExpectBegin().WillReturnError(failure)
			} else {
				m.ExpectBegin()
				current := model.New
				if phase == "terminal" {
					current = model.Processed
				}
				if phase == "lock" {
					m.ExpectQuery(lockOrderQuery).WillReturnError(failure)
				} else {
					m.ExpectQuery(lockOrderQuery).WithArgs("00").WillReturnRows(sqlmock.NewRows([]string{"user_id", "status"}).AddRow(1, current))
				}
				if phase != "lock" && phase != "terminal" {
					if value != nil && *value > 0 {
						expect := m.ExpectExec(creditQuery).WithArgs(int64(1), int64(500), int64(math.MaxInt64-500))
						switch phase {
						case "credit":
							expect.WillReturnError(failure)
						case "overflow":
							expect.WillReturnResult(sqlmock.NewResult(0, 0))
						default:
							expect.WillReturnResult(sqlmock.NewResult(0, 1))
						}
					}
					if phase != "credit" && phase != "overflow" {
						expect := m.ExpectExec(updateOrderQuery).WithArgs("00", status, value)
						if phase == "update" {
							expect.WillReturnError(failure)
						} else {
							expect.WillReturnResult(sqlmock.NewResult(0, 1))
							if phase == "commit" {
								m.ExpectCommit().WillReturnError(failure)
							} else {
								m.ExpectCommit()
							}
						}
					}
				}
				if phase == "terminal" || phase == "lock" || phase == "credit" || phase == "overflow" || phase == "update" {
					m.ExpectRollback()
				}
			}
			err := p.Apply(context.Background(), "00", status, value)
			wantOK := phase == "ok" || phase == "terminal" || phase == "zero" || phase == "missing" || phase == "pending"
			if (err == nil) != wantOK {
				t.Fatalf("apply %v", err)
			}
		})
	}
	p, _ := mockStore(t)
	negative := model.Money(-1)
	if err := p.Apply(context.Background(), "0", "UNKNOWN", nil); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background(), "0", model.Processed, &negative); !errors.Is(err, model.ErrInvalidInput) {
		t.Fatal(err)
	}
}
