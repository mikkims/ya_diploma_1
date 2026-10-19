package accrual

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mikkims/ya_diploma_1/internal/model"
)

// Result содержит проверенный статус и необязательное точное начисление.
type Result struct {
	Order   string       `json:"order"`
	Status  string       `json:"status"`
	Accrual *model.Money `json:"accrual"`
}

// RateLimitError сообщает обязательную паузу всех запросов к начислениям.
type RateLimitError struct{ RetryAfter time.Duration }

// Error возвращает безопасное описание ограничения частоты.
func (e *RateLimitError) Error() string { return "accrual rate limit" }

// HTTPStatus возвращает код ограничения частоты без данных ответа.
func (e *RateLimitError) HTTPStatus() int { return http.StatusTooManyRequests }

type statusError int

// Error описывает только HTTP-статус внешнего сервиса.
func (e statusError) Error() string { return fmt.Sprintf("accrual HTTP status %d", e) }

// HTTPStatus возвращает HTTP-статус для безопасной диагностики.
func (e statusError) HTTPStatus() int { return int(e) }

// Client ограничивает длительность и размер ответа внешнего API.
type Client struct {
	base string
	http *http.Client
}

// New создаёт клиент без перенаправлений и с таймаутом пять секунд.
func New(base string) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid accrual URL")
	}
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Fetch получает результат одного заказа; HTTP 204 возвращает пустой статус.
func (c *Client) Fetch(ctx context.Context, number string) (result Result, err error) {
	if !model.ValidOrder(number) {
		return Result{}, model.ErrInvalidOrder
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/orders/"+number, nil)
	if err != nil {
		return Result{}, fmt.Errorf("create accrual request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("request accrual: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			result = Result{}
			err = errors.Join(err, fmt.Errorf("close accrual response: %w", closeErr))
		}
	}()
	if resp.StatusCode == http.StatusNoContent {
		return Result{}, nil
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return Result{}, &RateLimitError{RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
	if resp.StatusCode != http.StatusOK {
		return Result{}, statusError(resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil {
		return Result{}, fmt.Errorf("read accrual response: %w", err)
	}
	if len(data) > 65536 {
		return Result{}, fmt.Errorf("accrual response too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("decode accrual response: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF || result.Order != number {
		return Result{}, fmt.Errorf("invalid accrual response")
	}
	switch result.Status {
	case "REGISTERED", model.Processing:
		result.Status = model.Processing
		result.Accrual = nil
	case model.Invalid:
		result.Accrual = nil
	case model.Processed:
	default:
		return Result{}, fmt.Errorf("unknown accrual status")
	}
	return result, nil
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds > math.MaxInt64/int64(time.Second) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return time.Second
}
