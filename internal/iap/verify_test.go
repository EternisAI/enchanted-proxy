package iap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

// testCert issues a certificate, self-signed when parent is nil, carrying the
// given extensions.
func testCert(t *testing.T, name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, ca bool, oids ...asn1.ObjectIdentifier) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  ca,
		BasicConstraintsValid: true,
	}
	for _, oid := range oids {
		tmpl.ExtraExtensions = append(tmpl.ExtraExtensions, pkix.Extension{Id: oid, Value: []byte{0x05, 0x00}})
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key
}

// jwsWith is a signed-transaction-shaped token whose header names the
// certificates; the checks under test read only the header.
func jwsWith(t *testing.T, certs ...*x509.Certificate) string {
	t.Helper()
	var x5c []string
	for _, c := range certs {
		x5c = append(x5c, base64.StdEncoding.EncodeToString(c.Raw))
	}
	header, err := json.Marshal(map[string]any{"alg": "ES256", "x5c": x5c})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + ".e30.c2ln"
}

func TestCheckSigningCertificates(t *testing.T) {
	root, rootKey := testCert(t, "root", nil, nil, true)
	intermediate, intermediateKey := testCert(t, "intermediate", root, rootKey, true, oidAppleIntermediate)
	plainIntermediate, plainKey := testCert(t, "other intermediate", root, rootKey, true)
	leaf, _ := testCert(t, "app store", intermediate, intermediateKey, false, oidAppStoreSigning)
	otherLeaf, _ := testCert(t, "another service", intermediate, intermediateKey, false)
	leafUnderPlain, _ := testCert(t, "app store", plainIntermediate, plainKey, false, oidAppStoreSigning)

	tests := []struct {
		name string
		jws  string
		ok   bool
	}{
		{"the App Store's chain", jwsWith(t, leaf, intermediate, root), true},
		{"leaf and intermediate only", jwsWith(t, leaf, intermediate), true},
		{"another certificate under the same intermediate", jwsWith(t, otherLeaf, intermediate, root), false},
		{"the App Store's marker under another intermediate", jwsWith(t, leafUnderPlain, plainIntermediate, root), false},
		{"leaf alone", jwsWith(t, leaf), false},
		{"not a token", "garbage", false},
		{"header not JSON", base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".e30.c2ln", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkSigningCertificates(tt.jws)
			if tt.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// The header of a transaction the App Store signed: its leaf is the App Store's
// receipt signing certificate and its intermediate Apple's WWDR G6. The check
// must accept it.
func TestCheckSigningCertificatesAcceptsTheAppStoresChain(t *testing.T) {
	header, err := os.ReadFile("testdata/app_store_jws_header.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSigningCertificates(strings.TrimSpace(string(header)) + ".e30.c2ln"); err != nil {
		t.Fatalf("the App Store's own chain was refused: %v", err)
	}
}
