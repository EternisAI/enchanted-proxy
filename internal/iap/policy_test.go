package iap

import (
	"slices"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	appstore "github.com/richzw/appstore"
)

var testNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func testPolicy() Policy {
	return Policy{
		BundleID:            "com.example.app",
		SandboxEmailDomains: []string{"example.com"},
		SandboxEmails:       []string{"tester@elsewhere.test"},
		MaxAccounts:         5,
	}
}

func lifetimeTx() *appstore.JWSTransaction {
	return &appstore.JWSTransaction{
		TransactionID: "1001", OriginalTransactionId: "1000", BundleID: "com.example.app",
		ProductID: "silo.plus.lifetime", Environment: appstore.Production, InAppOwnershipType: "PURCHASED",
	}
}

func subscriptionTx() *appstore.JWSTransaction {
	tx := lifetimeTx()
	tx.ProductID = "silo.pro.monthly"
	tx.ExpiresDate = testNow.Add(24 * time.Hour).UnixMilli()
	return tx
}

func identity(email string, verified bool) *auth.Identity {
	return &auth.Identity{UserID: "u", SignInProvider: auth.ProviderGoogle, Email: email, EmailVerified: verified}
}

func TestPolicyCheck(t *testing.T) {
	with := func(base func() *appstore.JWSTransaction, change func(*appstore.JWSTransaction)) *appstore.JWSTransaction {
		tx := base()
		change(tx)
		return tx
	}
	tests := []struct {
		name     string
		policy   func(*Policy)
		tx       *appstore.JWSTransaction
		identity *auth.Identity
		want     string // refusal reason, or "" when accepted
		tier     tiers.Tier
	}{
		{name: "lifetime purchase", tx: lifetimeTx(), want: "", tier: tiers.TierPlus},
		{name: "active subscription", tx: subscriptionTx(), want: "", tier: tiers.TierPro},
		{name: "another app", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.BundleID = "com.example.other" }), want: refusedBundle},
		{name: "no bundle configured", policy: func(p *Policy) { p.BundleID = "" }, tx: lifetimeTx(), want: refusedBundle},
		{name: "sandbox, verified email on the domain", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.Environment = appstore.Sandbox }),
			identity: identity("Someone@Example.com", true), want: "", tier: tiers.TierPlus},
		{name: "sandbox, unverified email on the domain", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.Environment = appstore.Sandbox }),
			identity: identity("someone@example.com", false), want: refusedSandbox},
		{name: "sandbox, listed tester", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.Environment = appstore.Sandbox }),
			identity: identity("tester@elsewhere.test", true), want: "", tier: tiers.TierPlus},
		{name: "sandbox, other domain", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.Environment = appstore.Sandbox }),
			identity: identity("someone@example.org", true), want: refusedSandbox},
		{name: "sandbox, domain only as a suffix", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.Environment = appstore.Sandbox }),
			identity: identity("someone@notexample.com", true), want: refusedSandbox},
		{name: "sandbox, no identity", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.Environment = appstore.Sandbox }), want: refusedSandbox},
		{name: "local StoreKit testing", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.Environment = "Xcode" }), want: refusedEnvironment},
		{name: "unknown product", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.ProductID = "silo.plus.lifetime.promo" }), want: refusedProduct},
		{name: "test product refused by default", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.ProductID = "silo.plus.lifetime.development" }), want: refusedProduct},
		{name: "test product where allowed", policy: func(p *Policy) { p.TestProducts = true },
			tx: with(subscriptionTx, func(tx *appstore.JWSTransaction) { tx.ProductID = "silo.pro.yearly.staging" }), want: "", tier: tiers.TierPro},
		{name: "unknown test product where allowed", policy: func(p *Policy) { p.TestProducts = true },
			tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.ProductID = "silo.max.yearly.staging" }), want: refusedProduct},
		{name: "family shared", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.InAppOwnershipType = "FAMILY_SHARED" }), want: refusedFamilyShared},
		{name: "upgraded", tx: with(subscriptionTx, func(tx *appstore.JWSTransaction) { tx.IsUpgraded = true }), want: refusedUpgraded},
		{name: "revoked", tx: with(lifetimeTx, func(tx *appstore.JWSTransaction) { tx.RevocationDate = testNow.Add(-time.Hour).UnixMilli() }), want: refusedRevoked},
		{name: "expired subscription", tx: with(subscriptionTx, func(tx *appstore.JWSTransaction) { tx.ExpiresDate = testNow.Add(-time.Minute).UnixMilli() }), want: refusedExpired},
		{name: "subscription without expiry", tx: with(subscriptionTx, func(tx *appstore.JWSTransaction) { tx.ExpiresDate = 0 }), want: refusedExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testPolicy()
			if tt.policy != nil {
				tt.policy(&p)
			}
			prod, refusal := p.check(tt.tx, tt.identity, testNow)
			switch {
			case tt.want == "" && refusal != nil:
				t.Fatalf("refused (%s), want accepted", refusal.Reason)
			case tt.want != "" && refusal == nil:
				t.Fatalf("accepted as %s, want refused (%s)", prod.tier, tt.want)
			case tt.want != "" && refusal.Reason != tt.want:
				t.Fatalf("refused for %s, want %s", refusal.Reason, tt.want)
			case tt.want == "" && prod.tier != tt.tier:
				t.Fatalf("grants %s, want %s", prod.tier, tt.tier)
			case refusal != nil && refusal.Message == "":
				t.Fatal("a refusal needs a sentence for the app")
			}
		})
	}
}

func TestLifetimePurchasesGetAFarExpiry(t *testing.T) {
	prod, _ := testPolicy().check(lifetimeTx(), nil, testNow)
	if got := prod.expiry(lifetimeTx()); !got.Equal(lifetimeExpiry) {
		t.Fatalf("lifetime expiry %v", got)
	}
	tx := subscriptionTx()
	prod, _ = testPolicy().check(tx, nil, testNow)
	if got := prod.expiry(tx); !got.Equal(time.UnixMilli(tx.ExpiresDate)) {
		t.Fatalf("subscription expiry %v, want the transaction's", got)
	}
}

func TestLoadPolicy(t *testing.T) {
	t.Setenv("APPSTORE_SANDBOX_EMAIL_DOMAINS", " Example.com, @example.org ,")
	t.Setenv("APPSTORE_SANDBOX_EMAILS", "A@B.test")
	t.Setenv("APPSTORE_TEST_PRODUCTS", "TRUE")
	t.Setenv("APPSTORE_PURCHASE_MAX_ACCOUNTS", "1") // below the routine two: ignored
	p := LoadPolicy()
	if !slices.Equal(p.SandboxEmailDomains, []string{"example.com", "example.org"}) ||
		!slices.Equal(p.SandboxEmails, []string{"a@b.test"}) || !p.TestProducts || p.MaxAccounts != 5 {
		t.Fatalf("policy %+v", p)
	}

	t.Setenv("APPSTORE_TEST_PRODUCTS", "")
	t.Setenv("APPSTORE_PURCHASE_MAX_ACCOUNTS", "8")
	if p := LoadPolicy(); p.TestProducts || p.MaxAccounts != 8 {
		t.Fatalf("policy %+v", p)
	}
}
