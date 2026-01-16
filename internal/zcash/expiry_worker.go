package zcash

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"

	"github.com/eternisai/enchanted-proxy/internal/logger"
	pgdb "github.com/eternisai/enchanted-proxy/internal/storage/pg/sqlc"
)

// ExpiryWorker marks old pending invoices as expired.
type ExpiryWorker struct {
	queries         pgdb.Querier
	firestoreClient *firestore.Client
	logger          *logger.Logger
	interval        time.Duration
	batchSize       int32
}

func NewExpiryWorker(queries pgdb.Querier, firestoreClient *firestore.Client, logger *logger.Logger) *ExpiryWorker {
	return &ExpiryWorker{
		queries:         queries,
		firestoreClient: firestoreClient,
		logger:          logger,
		interval:        1 * time.Hour,
		batchSize:       100,
	}
}

// Run starts the expiry worker loop.
func (w *ExpiryWorker) Run(ctx context.Context) {
	w.logger.Info("starting zcash invoice expiry worker", "interval", w.interval)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	// Run immediately on startup
	w.expireInvoices(ctx)

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("zcash invoice expiry worker stopped")
			return
		case <-ticker.C:
			w.expireInvoices(ctx)
		}
	}
}

func (w *ExpiryWorker) expireInvoices(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		invoices, err := w.queries.GetExpiredPendingInvoices(ctx, w.batchSize)
		if err != nil {
			w.logger.Error("failed to get expired invoices", "error", err.Error())
			return
		}

		if len(invoices) == 0 {
			return
		}

		w.logger.Info("expiring old invoices", "count", len(invoices))

		for _, inv := range invoices {
			// Update DB
			if err := w.queries.UpdateZcashInvoiceToExpired(ctx, inv.ID); err != nil {
				w.logger.Error("failed to expire invoice", "error", err.Error(), "invoice_id", inv.ID.String())
				continue
			}

			// Update Firestore
			if w.firestoreClient != nil {
				_, err := w.firestoreClient.Collection("zcash_invoices").Doc(inv.ID.String()).Update(ctx, []firestore.Update{
					{Path: "status", Value: "expired"},
					{Path: "updated_at", Value: firestore.ServerTimestamp},
				})
				if err != nil {
					w.logger.Error("failed to update Firestore for expired invoice", "error", err.Error(), "invoice_id", inv.ID.String())
				}
			}

			w.logger.Info("invoice expired", "invoice_id", inv.ID.String(), "user_id", inv.UserID)
		}

		// If we got fewer than batch size, we're done
		if int32(len(invoices)) < w.batchSize {
			return
		}
	}
}
