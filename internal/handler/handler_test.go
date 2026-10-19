package handler

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikkims/ya_diploma_1/internal/auth"
	"github.com/mikkims/ya_diploma_1/internal/model"
	"github.com/mikkims/ya_diploma_1/internal/service"
	"github.com/rs/zerolog"
)

type testStore struct {
	users   map[string]model.User
	orders  map[string]int64
	amounts map[int64]model.Balance
	history map[int64][]model.Withdrawal
	failure error
}

func newTestStore() *testStore {
	return &testStore{users: map[string]model.User{}, orders: map[string]int64{}, amounts: map[int64]model.Balance{}, history: map[int64][]model.Withdrawal{}}
}
func (s *testStore) CreateUser(_ context.Context, login, hash string) (int64, error) {
	if s.failure != nil {
		return 0, s.failure
	}
	if _, ok := s.users[login]; ok {
		return 0, model.ErrLoginTaken
	}
	id := int64(len(s.users) + 1)
	s.users[login] = model.User{ID: id, Login: login, PasswordHash: hash}
	return id, nil
}
func (s *testStore) UserByLogin(_ context.Context, login string) (model.User, error) {
	if s.failure != nil {
		return model.User{}, s.failure
	}
	u, ok := s.users[login]
	if !ok {
		return u, model.ErrUnauthorized
	}
	return u, nil
}
func (s *testStore) AddOrder(_ context.Context, id int64, number string) (bool, error) {
	if s.failure != nil {
		return false, s.failure
	}
	if owner, ok := s.orders[number]; ok {
		if owner != id {
			return false, model.ErrOrderOwned
		}
		return false, nil
	}
	s.orders[number] = id
	return true, nil
}
func (s *testStore) Orders(_ context.Context, id int64) ([]model.Order, error) {
	if s.failure != nil {
		return nil, s.failure
	}
	var out []model.Order
	for n, owner := range s.orders {
		if owner == id {
			out = append(out, model.Order{Number: n, Status: model.New, UploadedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
		}
	}
	return out, nil
}
func (s *testStore) Balance(_ context.Context, id int64) (model.Balance, error) {
	return s.amounts[id], s.failure
}
func (s *testStore) Withdraw(_ context.Context, id int64, number string, m model.Money) error {
	if s.failure != nil {
		return s.failure
	}
	b := s.amounts[id]
	if b.Current < m {
		return model.ErrInsufficient
	}
	b.Current -= m
	b.Withdrawn += m
	s.amounts[id] = b
	s.history[id] = append(s.history[id], model.Withdrawal{Order: number, Sum: m, ProcessedAt: time.Now().UTC()})
	return nil
}
func (s *testStore) Withdrawals(_ context.Context, id int64) ([]model.Withdrawal, error) {
	return s.history[id], s.failure
}

func request(router http.Handler, method, path, body, contentType, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		r.Header.Set("Authorization", token)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	return w
}

func TestAPIFlow(t *testing.T) {
	s := newTestStore()
	a, err := auth.New("")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	router := NewRouter(service.New(s, s), a, zerolog.New(&logs))
	register := request(router, "POST", "/api/user/register", `{"login":"alice","password":"synthetic-password"}`, "application/json", "")
	if register.Code != 200 {
		t.Fatalf("register %d", register.Code)
	}
	cookies := register.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("cookie security")
	}
	token := register.Header().Get("Authorization")
	if token == "" {
		t.Fatal("missing token")
	}
	if strings.Contains(s.users["alice"].PasswordHash, "synthetic-password") {
		t.Fatal("password not hashed")
	}
	if got := request(router, "POST", "/api/user/register", `{"login":"alice","password":"synthetic-password"}`, "application/json", "").Code; got != 409 {
		t.Fatalf("duplicate %d", got)
	}
	for _, tt := range []struct {
		body   string
		status int
	}{{`{"login":"alice","password":"synthetic-password"}`, 200}, {`{"login":"alice","password":"wrong"}`, 401},
		{`{"login":"missing","password":"wrong"}`, 401}, {`{"login":"","password":"x"}`, 400},
		{`{"login":"alice","password":"` + strings.Repeat("x", 73) + `"}`, 400}} {
		if got := request(router, "POST", "/api/user/login", tt.body, "application/json", "").Code; got != tt.status {
			t.Errorf("login status %d want %d", got, tt.status)
		}
	}
	for _, tt := range []struct {
		method, path, body, media string
		status                    int
	}{
		{"GET", "/api/user/orders", "", "", 204},
		{"GET", "/api/user/withdrawals", "", "", 204},
		{"GET", "/api/user/balance", "", "", 200},
		{"POST", "/api/user/orders", "12345678904", "text/plain", 422},
		{"POST", "/api/user/orders", "12345678903", "application/json", 400},
		{"POST", "/api/user/orders", "", "text/plain", 400},
		{"POST", "/api/user/orders", "00012345678903", "text/plain", 202},
		{"POST", "/api/user/orders", "00012345678903", "text/plain", 200},
		{"GET", "/api/user/orders", "", "", 200},
		{"POST", "/api/user/balance/withdraw", `{"order":"0","sum":1}`, "application/json", 402},
		{"POST", "/api/user/balance/withdraw", `{"order":"1","sum":1}`, "application/json", 422},
		{"POST", "/api/user/balance/withdraw", `{"order":"0","sum":0}`, "application/json", 400},
		{"POST", "/api/user/balance/withdraw", `{"order":"0","sum":0.001}`, "application/json", 400},
	} {
		if got := request(router, tt.method, tt.path, tt.body, tt.media, token).Code; got != tt.status {
			t.Errorf("%s %s got %d want %d", tt.method, tt.path, got, tt.status)
		}
	}
	second := request(router, "POST", "/api/user/register", `{"login":"bob","password":"synthetic"}`, "application/json", "").Header().Get("Authorization")
	if got := request(router, "POST", "/api/user/orders", "00012345678903", "text/plain", second).Code; got != 409 {
		t.Fatalf("foreign order %d", got)
	}
	if got := request(router, "GET", "/api/user/orders", "", "", second).Code; got != 204 {
		t.Fatal("foreign list leaked")
	}
	s.amounts[1] = model.Balance{Current: 10050}
	if got := request(router, "POST", "/api/user/balance/withdraw", `{"order":"0","sum":10.25}`, "application/json", token).Code; got != 200 {
		t.Fatalf("withdraw %d", got)
	}
	balance := request(router, "GET", "/api/user/balance", "", "", token)
	if strings.TrimSpace(balance.Body.String()) != `{"current":90.25,"withdrawn":10.25}` {
		t.Fatalf("balance %s", balance.Body.String())
	}
	history := request(router, "GET", "/api/user/withdrawals", "", "", token)
	var withdrawals []model.Withdrawal
	if err := json.Unmarshal(history.Body.Bytes(), &withdrawals); err != nil || len(withdrawals) != 1 || withdrawals[0].Sum != 1025 {
		t.Fatalf("history %v", err)
	}
	if got := request(router, "GET", "/api/user/withdrawals", "", "", second).Code; got != 204 {
		t.Fatal("foreign history leaked")
	}
	if body := request(router, "GET", "/api/user/balance", "", "", second).Body.String(); body != `{"current":0.00,"withdrawn":0.00}` {
		t.Fatal("foreign balance leaked")
	}
	r := httptest.NewRequest("GET", "/api/user/balance", nil)
	r.AddCookie(cookies[0])
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("cookie auth")
	}
	if strings.Contains(logs.String(), "synthetic") || strings.Contains(logs.String(), cookies[0].Value) || strings.Contains(logs.String(), "alice") {
		t.Fatal("sensitive logs")
	}
}

func TestAPIRejectsMalformedAndUnauthorized(t *testing.T) {
	s := newTestStore()
	a, _ := auth.New("")
	token, _ := a.Token(1)
	var logs bytes.Buffer
	router := NewRouter(service.New(s, s), a, zerolog.New(&logs))
	for _, path := range []string{"/api/user/orders", "/api/user/balance", "/api/user/withdrawals"} {
		for _, header := range []string{"", "Bearer invalid", "Basic abc", "Bearer a b"} {
			if got := request(router, "GET", path, "", "", header).Code; got != 401 {
				t.Errorf("auth %d", got)
			}
		}
	}
	for _, path := range []string{"/api/user/orders", "/api/user/balance/withdraw"} {
		if got := request(router, "POST", path, "broken", "application/json", "").Code; got != 401 {
			t.Fatal("unauthorized mutation")
		}
	}
	for _, body := range []string{"", `null`, `{}`, `{"login":"a","password":"x","extra":1}`, `{"login":"a","password":"x"} {}`, `{"login":"a","password":1}`, strings.Repeat("x", 16385)} {
		if got := request(router, "POST", "/api/user/register", body, "application/json", "").Code; got != 400 {
			t.Errorf("malformed %d", got)
		}
	}
	if got := request(router, "POST", "/api/user/register", `{}`, "text/plain", "").Code; got != 400 {
		t.Fatal("content type")
	}
	if got := request(router, "POST", "/api/user/orders", strings.Repeat("0", 16385), "text/plain", "Bearer "+token).Code; got != 400 {
		t.Fatal("order bound")
	}
	s.failure = errors.New("synthetic-internal-secret")
	for _, tt := range []struct{ method, path, body, media string }{{"POST", "/api/user/register", `{"login":"a","password":"x"}`, "application/json"},
		{"POST", "/api/user/login", `{"login":"a","password":"x"}`, "application/json"}, {"POST", "/api/user/orders", "0", "text/plain"},
		{"GET", "/api/user/orders", "", ""}, {"GET", "/api/user/balance", "", ""}, {"GET", "/api/user/withdrawals", "", ""},
		{"POST", "/api/user/balance/withdraw", `{"order":"0","sum":1}`, "application/json"}} {
		w := request(router, tt.method, tt.path, tt.body, tt.media, "Bearer "+token)
		if w.Code != 500 || strings.Contains(w.Body.String(), "secret") {
			t.Errorf("internal error %s %d", tt.path, w.Code)
		}
		if !strings.Contains(logs.String(), `"operation":"`+tt.path+`"`) || !strings.Contains(logs.String(), `"category":"internal"`) || strings.Contains(logs.String(), "synthetic-internal-secret") {
			t.Fatal("missing or sensitive API diagnostics")
		}
	}
}

func TestGzipAPI(t *testing.T) {
	s := newTestStore()
	a, _ := auth.New("")
	router := NewRouter(service.New(s, s), a, zerolog.Nop())
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write([]byte(`{"login":"gzip","password":"test"}`)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/user/register", &b)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("gzip request %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/api/user/balance", nil)
	r.Header.Set("Authorization", w.Header().Get("Authorization"))
	r.Header.Set("Accept-Encoding", "gzip")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("gzip response")
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil || !json.Valid(data) {
		t.Fatal("compressed JSON")
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	b.Reset()
	zw = gzip.NewWriter(&b)
	_, err = zw.Write(bytes.Repeat([]byte("x"), 20000))
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	r = httptest.NewRequest("POST", "/api/user/register", &b)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Encoding", "gzip")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("decompressed size bound")
	}
}
