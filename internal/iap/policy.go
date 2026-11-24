package iap

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/auth"
	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/tiers"
	appstore "github.com/richzw/appstore"
)

// product is what a product ID grants.
type product struct {
	tier     tiers.Tier
	lifetime bool
}

// products are the App Store products and the plans they grant. Anything else
// is refused.
var products = map[string]product{
	"silo.plus.lifetime": {tier: tiers.TierPlus, lifetime: true},
	"silo.pro.weekly":    {tier: tiers.TierPro},
	"silo.pro.monthly":   {tier: tiers.TierPro},
	"silo.pro.yearly":    {tier: tiers.TierPro},
}

// testProductSuffixes mark the products of the staging and development
// builds, accepted only where Policy.TestProducts is set.
var testProductSuffixes = []string{".staging", ".development"}

// Policy is what the server accepts as a purchase.
type Policy struct {
	// BundleID is the app whose purchases are accepted.
	BundleID string
	// SandboxEmailDomains and SandboxEmails name the accounts whose sandbox
	// purchases are accepted, by the account's verified email. Lower case.
	SandboxEmailDomains []string
	SandboxEmails       []string
	// TestProducts accepts the staging and development builds' products.
	TestProducts bool
	// MaxAccounts is how many accounts one purchase may reach in
	// accountWindow; the next one blocks it.
	MaxAccounts int
}

// accountWindow is the period over which the accounts a purchase reaches are
// counted.
const accountWindow = 30 * 24 * time.Hour

// signalAccounts is how many accounts in accountWindow make a purchase worth
// a log line, before it is blocked.
const signalAccounts = 3

// LoadPolicy reads the policy from the configuration and the environment.
func LoadPolicy() Policy {
	maxAccounts := 5
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("APPSTORE_PURCHASE_MAX_ACCOUNTS"))); err == nil && v >= 2 {
		maxAccounts = v
	}
	bundleID := ""
	if config.AppConfig != nil {
		bundleID = config.AppConfig.AppStoreBundleID
	}
	domains := envList("APPSTORE_SANDBOX_EMAIL_DOMAINS", "eternis.ai")
	for i, d := range domains {
		domains[i] = strings.TrimPrefix(d, "@")
	}
	return Policy{
		BundleID:            bundleID,
		SandboxEmailDomains: domains,
		SandboxEmails:       envList("APPSTORE_SANDBOX_EMAILS", ""),
		TestProducts:        strings.EqualFold(strings.TrimSpace(os.Getenv("APPSTORE_TEST_PRODUCTS")), "true"),
		MaxAccounts:         maxAccounts,
	}
}

// envList reads a comma-separated list, lower case, with def when unset.
func envList(name, def string) []string {
	raw, ok := os.LookupEnv(name)
	if !ok {
		raw = def
	}
	var out []string
	for _, v := range strings.Split(raw, ",") {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Refusal is a purchase the server does not accept, with a sentence the app
// can show.
type Refusal struct {
	Reason  string
	Message string
}

func (r *Refusal) Error() string { return "purchase refused: " + r.Reason }

// Refusal reasons.
const (
	refusedBundle       = "bundle"
	refusedSandbox      = "sandbox"
	refusedEnvironment  = "environment"
	refusedProduct      = "product"
	refusedFamilyShared = "family_shared"
	refusedUpgraded     = "upgraded"
	refusedRevoked      = "revoked"
	refusedExpired      = "expired"
	refusedUnknown      = "unknown_at_apple"
)

func refuse(reason, message string) *Refusal {
	return &Refusal{Reason: reason, Message: message}
}

// sandboxAllowed reports whether the account may use sandbox purchases: its
// email must be verified, and on an allowed domain or in the list.
func (p Policy) sandboxAllowed(identity *auth.Identity) bool {
	if identity == nil {
		return false
	}
	email, ok := identity.VerifiedEmail()
	if !ok {
		return false
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if slices.Contains(p.SandboxEmails, email) {
		return true
	}
	at := strings.LastIndex(email, "@")
	return at > 0 && slices.Contains(p.SandboxEmailDomains, email[at+1:])
}

// product returns what a product ID grants under the policy.
func (p Policy) product(productID string) (product, bool) {
	if prod, ok := products[productID]; ok {
		return prod, true
	}
	if !p.TestProducts {
		return product{}, false
	}
	for _, suffix := range testProductSuffixes {
		if base, ok := strings.CutSuffix(productID, suffix); ok {
			prod, known := products[base]
			return prod, known
		}
	}
	return product{}, false
}

// check decides whether a signed transaction, already verified as Apple's,
// grants a plan. It returns what it grants, or a refusal. A revoked
// transaction is refused with refusedRevoked; the caller blocks the purchase.
func (p Policy) check(tx *appstore.JWSTransaction, identity *auth.Identity, now time.Time) (product, *Refusal) {
	if p.BundleID == "" || tx.BundleID != p.BundleID {
		return product{}, refuse(refusedBundle, "This purchase is for another app.")
	}
	switch tx.Environment {
	case appstore.Production:
	case appstore.Sandbox:
		if !p.sandboxAllowed(identity) {
			return product{}, refuse(refusedSandbox, "Test purchases are not accepted for this account.")
		}
	default:
		return product{}, refuse(refusedEnvironment, "This purchase was not made in the App Store.")
	}
	prod, ok := p.product(tx.ProductID)
	if !ok {
		return product{}, refuse(refusedProduct, "This product is not available.")
	}
	if strings.EqualFold(tx.InAppOwnershipType, "FAMILY_SHARED") {
		return product{}, refuse(refusedFamilyShared, "Purchases shared through Family Sharing are not supported. Buy the plan from this Apple ID to use it.")
	}
	if tx.IsUpgraded {
		// The upgrade's own transaction carries the plan.
		return product{}, refuse(refusedUpgraded, "This subscription was replaced by an upgrade.")
	}
	if tx.RevocationDate > 0 {
		return product{}, refuse(refusedRevoked, "This purchase was refunded or revoked.")
	}
	if !prod.lifetime {
		if tx.ExpiresDate <= 0 || !time.UnixMilli(tx.ExpiresDate).After(now) {
			return product{}, refuse(refusedExpired, "This subscription has expired.")
		}
	}
	return prod, nil
}

// expiry is when the plan a transaction grants ends. A lifetime purchase is
// given a date far ahead, as before, so the existing expiry checks pass.
func (prod product) expiry(tx *appstore.JWSTransaction) time.Time {
	if prod.lifetime {
		return lifetimeExpiry
	}
	return time.UnixMilli(tx.ExpiresDate).UTC()
}

var lifetimeExpiry = time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC)

// String names the policy's settings for the startup log; it holds no email.
func (p Policy) String() string {
	return fmt.Sprintf("bundle=%q sandbox_domains=%v sandbox_emails=%d test_products=%v max_accounts=%d",
		p.BundleID, p.SandboxEmailDomains, len(p.SandboxEmails), p.TestProducts, p.MaxAccounts)
}
