package accrual

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mikkims/ya_diploma_1/internal/logger"
	"github.com/mikkims/ya_diploma_1/internal/model"
	"github.com/rs/zerolog"
)

// Store даёт worker доступ только к незавершённым заказам и их результатам.
type Store interface {
	Pending(context.Context) ([]string, error)
	Apply(context.Context, string, string, *model.Money) error
}

// Run опрашивает заказы пулом воркеров до отмены и дожидается всех горутин.
func Run(ctx context.Context, store Store, client *Client, log zerolog.Logger) {
	for ctx.Err() == nil {
		queryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		numbers, err := store.Pending(queryCtx)
		cancel()
		if err != nil {
			logger.Failure(log.Warn(), "orders.pending", err).Msg("pending orders query failed")
		}
		jobs := make(chan string)
		var workers sync.WaitGroup
		for i := 0; i < 4 && i < len(numbers); i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for number := range jobs {
					if ctx.Err() != nil {
						return
					}
					result, err := client.Fetch(ctx, number)
					if ctx.Err() != nil {
						return
					}
					var limit *RateLimitError
					if errors.As(err, &limit) {
						logger.Failure(log.Warn(), "accrual.fetch", err).Msg("accrual rate limited")
						continue
					}
					if err != nil {
						logger.Failure(log.Warn(), "accrual.fetch", err).Msg("accrual request failed")
					}
					applyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
					err = store.Apply(applyCtx, number, result.Status, result.Accrual)
					cancel()
					if err != nil {
						logger.Failure(log.Warn(), "accrual.apply", err).Msg("accrual persistence failed")
					}
					if !wait(ctx, 200*time.Millisecond) {
						return
					}
				}
			}()
		}
		for _, number := range numbers {
			select {
			case <-ctx.Done():
				close(jobs)
				workers.Wait()
				return
			case jobs <- number:
			}
		}
		close(jobs)
		workers.Wait()
		if !wait(ctx, time.Second) {
			return
		}
	}
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
