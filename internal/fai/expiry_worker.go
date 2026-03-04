package fai

import (
	"context"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
)

// ExpiryWorker marks old pending FAI payment intents as expired.
type ExpiryWorker struct {
	queries   pgdb.Querier
	logger    *logger.Logger
	interval  time.Duration
	batchSize int32
}

func NewExpiryWorker(queries pgdb.Querier, logger *logger.Logger) *ExpiryWorker {
	return &ExpiryWorker{
		queries:   queries,
		logger:    logger,
		interval:  1 * time.Hour,
		batchSize: 100,
	}
}

// Run starts the expiry worker loop.
func (w *ExpiryWorker) Run(ctx context.Context) {
	w.logger.Info("starting FAI payment intent expiry worker", "interval", w.interval)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Run immediately on startup
	w.expireIntents(ctx)

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("FAI payment intent expiry worker stopped")
			return
		case <-ticker.C:
			w.expireIntents(ctx)
		}
	}
}

func (w *ExpiryWorker) expireIntents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		intents, err := w.queries.GetExpiredPendingFaiPaymentIntents(ctx, w.batchSize)
		if err != nil {
			w.logger.Error("failed to get expired FAI payment intents", "error", err.Error())
			return
		}

		if len(intents) == 0 {
			return
		}

		w.logger.Info("expiring old FAI payment intents", "count", len(intents))

		for _, intent := range intents {
			if err := w.queries.UpdateFaiPaymentIntentToExpired(ctx, intent.ID); err != nil {
				w.logger.Error("failed to expire FAI payment intent", "error", err.Error(), "id", intent.ID)
				continue
			}
			w.logger.Info("FAI payment intent expired", "id", intent.ID, "user_id", intent.UserID)
		}

		if int32(len(intents)) < w.batchSize {
			return
		}
	}
}
