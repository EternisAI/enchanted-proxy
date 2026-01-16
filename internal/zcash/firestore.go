package zcash

import (
	"context"
	"time"

	"cloud.google.com/go/firestore"
)

// ZcashInvoiceFirestore represents the Firestore document structure for ZCash invoices.
// Collection: /zcash_invoices/{invoiceId}
type ZcashInvoiceFirestore struct {
	UserID           string     `firestore:"user_id"`
	ProductID        string     `firestore:"product_id"`
	AmountZatoshis   int64      `firestore:"amount_zatoshis"`
	ZecAmount        float64    `firestore:"zec_amount"`
	PriceUSD         float64    `firestore:"price_usd"`
	ReceivingAddress string     `firestore:"receiving_address"`
	Status           string     `firestore:"status"`
	CreatedAt        time.Time  `firestore:"created_at"`
	UpdatedAt        time.Time  `firestore:"updated_at"`
	PaidAt           *time.Time `firestore:"paid_at,omitempty"`
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

// UpdateInvoiceStatusInFirestore updates the status and updated_at fields.
// If status is "paid", also sets paid_at.
func (s *Service) UpdateInvoiceStatusInFirestore(ctx context.Context, invoiceID, status string) error {
	if s.firestoreClient == nil {
		return nil
	}
	now := time.Now()
	updates := []firestore.Update{
		{Path: "status", Value: status},
		{Path: "updated_at", Value: now},
	}
	if status == "paid" {
		updates = append(updates, firestore.Update{Path: "paid_at", Value: now})
	}
	_, err := s.firestoreClient.Collection("zcash_invoices").Doc(invoiceID).Update(ctx, updates)
	return err
}
