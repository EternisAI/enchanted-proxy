package zcash

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
)

// ZcashInvoiceFirestore represents the Firestore document structure for ZCash invoices.
// Collection: /zcash_invoices/{invoiceId}
type ZcashInvoiceFirestore struct {
	UserID           string    `firestore:"user_id"`
	ProductID        string    `firestore:"product_id"`
	AmountZatoshis   int64     `firestore:"amount_zatoshis"`
	ZecAmount        float64   `firestore:"zec_amount"`
	PriceUSD         float64   `firestore:"price_usd"`
	ReceivingAddress string    `firestore:"receiving_address"`
	Status           string    `firestore:"status"`
	CreatedAt        time.Time `firestore:"created_at"`
	UpdatedAt        time.Time `firestore:"updated_at"`
}

// WriteInvoiceToFirestore creates or overwrites the invoice document.
// Collection: /zcash_invoices/{invoiceId}
func (s *Service) WriteInvoiceToFirestore(ctx context.Context, invoiceID string, data *ZcashInvoiceFirestore) error {
	if s.firestoreClient == nil {
		return nil
	}
	_, err := s.firestoreClient.Collection("zcash_invoices").Doc(invoiceID).Set(ctx, data)
	return err
}

// UpdateInvoiceStatusInFirestore updates just the status and updated_at fields.
func (s *Service) UpdateInvoiceStatusInFirestore(ctx context.Context, invoiceID, status string) error {
	if s.firestoreClient == nil {
		return nil
	}
	_, err := s.firestoreClient.Collection("zcash_invoices").Doc(invoiceID).Update(ctx, []firestore.Update{
		{Path: "status", Value: status},
		{Path: "updated_at", Value: time.Now()},
	})
	return err
}
