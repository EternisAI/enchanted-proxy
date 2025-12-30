package zcash

import (
	"testing"
)

func TestParseInvoiceID(t *testing.T) {
	tests := []struct {
		name      string
		invoiceID string
		wantErr   bool
		wantParts *invoiceParts
	}{
		{
			name:      "valid invoice with underscores in userID",
			invoiceID: "user_id_123__monthly_pro__1735254000",
			wantErr:   false,
			wantParts: &invoiceParts{
				userID:    "user_id_123",
				productID: "monthly_pro",
				timestamp: "1735254000",
			},
		},
		{
			name:      "valid simple invoice",
			invoiceID: "user123__product456__1735254000",
			wantErr:   false,
			wantParts: &invoiceParts{
				userID:    "user123",
				productID: "product456",
				timestamp: "1735254000",
			},
		},
		{
			name:      "invalid - missing parts",
			invoiceID: "user123__product456",
			wantErr:   true,
		},
		{
			name:      "invalid - too many parts",
			invoiceID: "user123__product456__timestamp__extra",
			wantErr:   false,
			wantParts: &invoiceParts{
				userID:    "user123",
				productID: "product456",
				timestamp: "timestamp__extra",
			},
		},
		{
			name:      "invalid - single underscore delimiter",
			invoiceID: "user123_product456_1735254000",
			wantErr:   true,
		},
		{
			name:      "invalid - empty string",
			invoiceID: "",
			wantErr:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseInvoiceID(tt.invoiceID)
			if tt.wantErr {
				if got != nil {
					t.Errorf("parseInvoiceID(%q) = %v, want nil", tt.invoiceID, got)
				}
				return
			}
			if got == nil {
				t.Errorf("parseInvoiceID(%q) = nil, want %v", tt.invoiceID, tt.wantParts)
				return
			}
			if got.userID != tt.wantParts.userID {
				t.Errorf("userID = %q, want %q", got.userID, tt.wantParts.userID)
			}
			if got.productID != tt.wantParts.productID {
				t.Errorf("productID = %q, want %q", got.productID, tt.wantParts.productID)
			}
			if got.timestamp != tt.wantParts.timestamp {
				t.Errorf("timestamp = %q, want %q", got.timestamp, tt.wantParts.timestamp)
			}
		})
	}
}
