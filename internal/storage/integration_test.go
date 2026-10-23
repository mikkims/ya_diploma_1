package storage

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mikkims/ya_diploma_1/internal/auth"
	"github.com/mikkims/ya_diploma_1/internal/handler"
	"github.com/mikkims/ya_diploma_1/internal/model"
	"github.com/mikkims/ya_diploma_1/internal/service"
	"github.com/mikkims/ya_diploma_1/migrations"
	"github.com/rs/zerolog"
)

const createTestDatabaseQuery = `
CREATE DATABASE %s
`
const dropTestDatabaseQuery = `
DROP DATABASE %s
`
const rejectTestWithdrawalQuery = `
ALTER TABLE withdrawals
ADD CONSTRAINT reject_test_withdrawal CHECK (order_number <> 'rollback')
`

const hashCollisionQuery = `
SELECT min(number), max(number)
FROM (
    SELECT g::text AS number, hashtext(g::text) AS hash
    FROM generate_series(1, 300000) AS g
) AS candidates
GROUP BY hash
HAVING count(*) > 1
LIMIT 1
`

const equalHashQuery = `
SELECT hashtext($1) = hashtext($2)
`

func longOrder(seed int64, size int) string {
	rng := rand.New(rand.NewSource(seed))
	data := make([]byte, size)
	data[0] = '0'
	for i := 1; i < size-1; i++ {
		data[i] = byte('0' + rng.Intn(10))
	}
	for check := byte('0'); check <= '9'; check++ {
		data[size-1] = check
		if model.ValidOrder(string(data)) {
			return string(data)
		}
	}
	panic("no Luhn check digit")
}

func TestPostgresLongOrders(t *testing.T) {
	db := temporaryPostgres(t)
	p := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	first, err := p.CreateUser(ctx, "long-first", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.CreateUser(ctx, "long-second", "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	router := handler.NewRouter(service.New(p, p), a, zerolog.Nop())
	token, err := a.Token(first)
	if err != nil {
		t.Fatal(err)
	}
	otherToken, err := a.Token(second)
	if err != nil {
		t.Fatal(err)
	}
	request := func(number, token string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/user/orders", strings.NewReader(number)).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w.Code
	}
	for _, size := range []int{16383, 16384} {
		number := longOrder(int64(size), size)
		if got := request(number, token); got != 202 {
			t.Fatalf("size %d: got %d", size, got)
		}
		if got := request(number, token); got != 200 {
			t.Fatalf("own duplicate: %d", got)
		}
		if got := request(number, otherToken); got != 409 {
			t.Fatalf("foreign duplicate: %d", got)
		}
	}
	if got := request(longOrder(17, 16385), token); got != 400 {
		t.Fatalf("body limit: %d", got)
	}
	number := longOrder(42, 16384)
	var created atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch code := request(number, token); code {
			case 202:
				created.Add(1)
			case 200:
			default:
				t.Errorf("concurrent insertion: %d", code)
			}
		}()
	}
	close(start)
	wg.Wait()
	if created.Load() != 1 {
		t.Fatal("expected exactly one creation")
	}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code := request(number, otherToken); code != 409 {
				t.Errorf("foreign concurrent duplicate: %d", code)
			}
		}()
	}
	wg.Wait()
	orders, err := p.Orders(ctx, first)
	if err != nil || len(orders) != 3 {
		t.Fatalf("long orders read: count %d, error %v", len(orders), err)
	}
	want := map[string]bool{longOrder(16383, 16383): true, longOrder(16384, 16384): true, number: true}
	for _, order := range orders {
		if !want[order.Number] || order.Number[0] != '0' {
			t.Fatal("stored number changed")
		}
		delete(want, order.Number)
	}
	if len(want) != 0 {
		t.Fatal("missing number")
	}
	var left, right string
	if err := db.QueryRow(ctx, hashCollisionQuery).Scan(&left, &right); err != nil {
		t.Fatal("collision search failed", err)
	}
	var equal bool
	if err := db.QueryRow(ctx, equalHashQuery, left, right).Scan(&equal); err != nil || !equal || left == right {
		t.Fatal("invalid collision fixture")
	}
	for _, n := range []string{left, right} {
		if ok, err := p.AddOrder(ctx, first, n); err != nil || !ok {
			t.Fatalf("different equal-hash strings must coexist: %v", err)
		}
		if ok, err := p.AddOrder(ctx, first, n); err != nil || ok {
			t.Fatalf("equal-hash repeat: %v", err)
		}
	}
}

func temporaryPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("GOPHERMART_TEST_DATABASE_URI")
	if dsn == "" {
		t.Skip("GOPHERMART_TEST_DATABASE_URI не задан: реальный PostgreSQL не проверен")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		t.Fatal("Для теста нужен PostgreSQL URI отдельного локального экземпляра")
	}
	q := u.Query()
	q.Set("connect_timeout", "5")
	u.RawQuery = q.Encode()
	admin, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatal("Не удалось создать тестовое подключение")
	}
	t.Cleanup(func() {
		admin.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	name := fmt.Sprintf("gophermart_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, fmt.Sprintf(createTestDatabaseQuery, name)); err != nil {
		t.Fatal("Не удалось создать временную БД; проверьте отдельный локальный PostgreSQL и CREATEDB")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, fmt.Sprintf(dropTestDatabaseQuery, name)); err != nil {
			t.Error("Не удалось удалить созданную тестом временную БД")
		}
	})
	u.Path = "/" + name
	db, err := pgxpool.New(context.Background(), u.String())
	if err != nil {
		t.Fatal("Не удалось подключиться к временной БД")
	}
	t.Cleanup(func() {
		db.Close()
	})
	if err := migrations.Up(ctx, db); err != nil {
		t.Fatal("Начальная миграция не прошла")
	}
	if err := migrations.Up(ctx, db); err != nil {
		t.Fatal("Повторная миграция не идемпотентна")
	}
	return db
}

func TestPostgresIntegration(t *testing.T) {
	db := temporaryPostgres(t)
	p := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := p.CreateUser(ctx, "alice", "synthetic-hash")
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.CreateUser(ctx, "bob", "synthetic-hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CreateUser(ctx, "alice", "another"); !errors.Is(err, model.ErrLoginTaken) {
		t.Fatal("login uniqueness")
	}
	var created atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := p.AddOrder(ctx, first, "00012345678903")
			if err != nil {
				t.Error(err)
			}
			if ok {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatal("concurrent duplicate orders")
	}
	if _, err := p.AddOrder(ctx, second, "00012345678903"); !errors.Is(err, model.ErrOrderOwned) {
		t.Fatal("foreign ownership")
	}
	if orders, err := p.Orders(ctx, second); err != nil || len(orders) != 0 {
		t.Fatal("foreign orders visible")
	}
	resumed := New(db)
	pending, err := resumed.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0] != "00012345678903" {
		t.Fatal("pending order not restored")
	}
	amount := model.Money(1250)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Apply(ctx, "00012345678903", model.Processed, &amount); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if b, err := p.Balance(ctx, first); err != nil || b.Current != 1250 {
		t.Fatalf("exactly once accrual: %+v %v", b, err)
	}
	if pending, err := p.Pending(ctx); err != nil || len(pending) != 0 {
		t.Fatal("final order still pending")
	}
	var successes atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := p.Withdraw(ctx, first, "18", 100)
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, model.ErrInsufficient) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	b, err := p.Balance(ctx, first)
	if err != nil || successes.Load() != 12 || b.Current != 50 || b.Withdrawn != 1200 {
		t.Fatalf("concurrent withdrawal: %+v successes %d err %v", b, successes.Load(), err)
	}
	history, err := p.Withdrawals(ctx, first)
	if err != nil || len(history) != 12 {
		t.Fatal("history count")
	}
	for i := 1; i < len(history); i++ {
		if history[i].ProcessedAt.After(history[i-1].ProcessedAt) {
			t.Fatal("history order")
		}
	}
	if history, err := p.Withdrawals(ctx, second); err != nil || len(history) != 0 {
		t.Fatal("foreign history visible")
	}
	if _, err := db.Exec(ctx, rejectTestWithdrawalQuery); err != nil {
		t.Fatal(err)
	}
	if err := p.Withdraw(ctx, first, "rollback", 25); err == nil {
		t.Fatal("expected history failure")
	}
	if after, err := p.Balance(ctx, first); err != nil || after != b {
		t.Fatal("withdrawal did not rollback")
	}
	for _, n := range []string{"18", "0000", strings.Repeat("0", 200)} {
		if _, err := p.AddOrder(ctx, first, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Apply(ctx, "18", model.Invalid, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, "18", model.Processed, &amount); err != nil {
		t.Fatal(err)
	}
	zero := model.Money(0)
	if err := p.Apply(ctx, "0000", model.Processed, &zero); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, strings.Repeat("0", 200), "", nil); err != nil {
		t.Fatal(err)
	}
	orders, err := p.Orders(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	for i, o := range orders {
		if i > 0 && o.UploadedAt.After(orders[i-1].UploadedAt) {
			t.Fatal("order sorting")
		}
		if o.Number == "0000" && (o.Accrual == nil || *o.Accrual != 0) {
			t.Fatal("zero disappeared")
		}
		if o.Number == "18" && (o.Status != model.Invalid || o.Accrual != nil) {
			t.Fatal("invalid is not final")
		}
	}
}
