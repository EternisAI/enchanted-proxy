package iap

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	appstore "github.com/richzw/appstore"
)

// Extensions Apple puts on the App Store's signing certificate and on the
// intermediate that issues it. The library verifies a signed transaction's
// chain up to Apple's root, but every certificate Apple issues chains there;
// these mark the App Store's.
var (
	oidAppStoreSigning   = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 11, 1}
	oidAppleIntermediate = asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 6, 2, 1}
)

// errNotAppStoreSigned is a signed transaction whose certificates are not the
// App Store's.
var errNotAppStoreSigned = errors.New("transaction is not signed by the App Store")

// checkSigningCertificates checks that a signed transaction names the App
// Store's signing certificate and intermediate. It reads the certificates the
// signature is checked against; the chain and the signature themselves are
// verified by the library.
func checkSigningCertificates(jws string) error {
	header, _, ok := strings.Cut(jws, ".")
	if !ok {
		return errNotAppStoreSigned
	}
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(header); err != nil {
			return errNotAppStoreSigned
		}
	}
	var h struct {
		X5c []string `json:"x5c"`
	}
	if err := json.Unmarshal(raw, &h); err != nil || len(h.X5c) < 2 {
		return errNotAppStoreSigned
	}
	certs := make([]*x509.Certificate, 2)
	for i := range certs {
		der, err := base64.StdEncoding.DecodeString(h.X5c[i])
		if err != nil {
			return errNotAppStoreSigned
		}
		if certs[i], err = x509.ParseCertificate(der); err != nil {
			return errNotAppStoreSigned
		}
	}
	if !hasExtension(certs[0], oidAppStoreSigning) || !hasExtension(certs[1], oidAppleIntermediate) {
		return errNotAppStoreSigned
	}
	return nil
}

func hasExtension(cert *x509.Certificate, oid asn1.ObjectIdentifier) bool {
	return slices.ContainsFunc(cert.Extensions, func(e pkix.Extension) bool { return e.Id.Equal(oid) })
}

// transactionParser verifies a signed transaction and returns its payload.
type transactionParser interface {
	ParseTransaction(jws string) (*appstore.JWSTransaction, error)
}

// appleParser verifies against Apple's root and the App Store's certificates.
type appleParser struct{ store *appstore.StoreClient }

func (p appleParser) ParseTransaction(jws string) (*appstore.JWSTransaction, error) {
	if err := checkSigningCertificates(jws); err != nil {
		return nil, err
	}
	tx, err := p.store.ParseNotificationV2TransactionInfo(jws)
	if err != nil {
		return nil, fmt.Errorf("invalid signed transaction: %w", err)
	}
	return tx, nil
}

// AppleAPI reads a transaction's current state from the App Store Server API.
type AppleAPI interface {
	// SignedTransaction returns the transaction as the App Store signs it now,
	// or an error wrapping errNotAtApple when the App Store does not know it.
	SignedTransaction(ctx context.Context, environment appstore.Environment, transactionID string) (string, error)
}

// errNotAtApple means the App Store does not know the transaction.
var errNotAtApple = errors.New("the App Store does not know this transaction")

// appleTimeout bounds one App Store Server API call.
const appleTimeout = 10 * time.Second

// storeAPI is the App Store Server API, production and sandbox.
type storeAPI struct {
	production *appstore.StoreClient
	sandbox    *appstore.StoreClient
}

func (a storeAPI) SignedTransaction(ctx context.Context, environment appstore.Environment, transactionID string) (string, error) {
	client := a.production
	if environment == appstore.Sandbox {
		client = a.sandbox
	}
	ctx, cancel := context.WithTimeout(ctx, appleTimeout)
	defer cancel()
	rsp, err := client.GetTransactionInfo(ctx, transactionID)
	if err != nil {
		var apiErr *appstore.Error
		if errors.As(err, &apiErr) {
			switch apiErr.ErrorCode() {
			case appstore.TransactionIdNotFoundError.ErrorCode(),
				appstore.OriginalTransactionIdNotFoundError.ErrorCode(),
				appstore.InvalidTransactionIdError.ErrorCode():
				return "", fmt.Errorf("%w: %v", errNotAtApple, err)
			}
		}
		return "", fmt.Errorf("failed to read the transaction from the App Store: %w", err)
	}
	if rsp == nil || rsp.SignedTransactionInfo == "" {
		return "", errors.New("the App Store returned no transaction")
	}
	return rsp.SignedTransactionInfo, nil
}
