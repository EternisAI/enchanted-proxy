package probe

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
	"github.com/eternisai/enchanted-proxy/internal/routing"
)

// testCA issues client certificates for the mutual TLS tests.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key}
}

// writeClientCert issues a client certificate with the given common name and
// writes it and its key as PEM files, replacing any earlier ones.
func (ca *testCA) writeClientCert(t *testing.T, certFile, keyFile, commonName string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newMutualTLSEndpoint starts a fake model endpoint that requires a client
// certificate from ca and records the common name of each client it serves.
func newMutualTLSEndpoint(t *testing.T, ca *testCA) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var clients []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		clients = append(clients, r.TLS.PeerCertificates[0].Subject.CommonName)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "OK"}}},
		})
	}))
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	server.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), clients...)
	}
}

// spkiPin is the base64 SHA-256 of a certificate's SubjectPublicKeyInfo.
func spkiPin(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// newMutualTLSWorker builds a worker whose endpoint has an unreachable base URL
// (as an enclave-only loopback URL is, seen from the prober) and whose probe
// settings point at probeURL with the given TLS settings.
func newMutualTLSWorker(probeURL string, tlsCfg *config.ProbeTLSConfig) *probeWorker {
	expected := "OK"
	probe := &routing.ProbeConfig{
		Prompt:           "Say OK",
		ExpectedResponse: &expected,
		MaxTokens:        16,
		Timeout:          5 * time.Second,
		BaseURL:          probeURL,
		TLS:              tlsCfg,
	}
	return &probeWorker{
		ctx: context.Background(),
		endpoint: &routing.ProviderConfig{
			BaseURL: "http://127.0.0.1:1/v1",
			Name:    "TestProvider",
			Model:   "pinned-model",
			APIType: config.APITypeChatCompletions,
		},
		probe:    probe,
		provider: "TestProvider",
		model:    "pinned-model",
		client:   newProbeHTTPClient(probe.Timeout, tlsCfg),
		logger:   testLogger(),
	}
}

// An endpoint behind mutual TLS with a self-signed server certificate is
// probed at its probe base URL, presenting the client certificate on disk and
// accepting the server only by its pinned key. The certificate is re-read for
// each probe, so a rotated one is used without restarting the prober.
func TestProbeUsesPinnedMutualTLS(t *testing.T) {
	ca := newTestCA(t)
	server, clients := newMutualTLSEndpoint(t, ca)

	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	ca.writeClientCert(t, certFile, keyFile, "first")

	w := newMutualTLSWorker(server.URL, &config.ProbeTLSConfig{
		ClientCertFile:   certFile,
		ClientKeyFile:    keyFile,
		ServerSPKISHA256: []string{spkiPin(server.Certificate())},
	})

	if result := w.runProbe(); !result.success {
		t.Fatalf("first probe failed: %v", result.err)
	}

	ca.writeClientCert(t, certFile, keyFile, "second")
	if result := w.runProbe(); !result.success {
		t.Fatalf("probe after rotating the client certificate failed: %v", result.err)
	}

	if got := clients(); len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("server saw clients %v, want [first second]", got)
	}
}

// A server whose key is not pinned is refused during the handshake, before
// any request is sent.
func TestProbeRefusesAnUnpinnedServer(t *testing.T) {
	ca := newTestCA(t)
	server, clients := newMutualTLSEndpoint(t, ca)

	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	ca.writeClientCert(t, certFile, keyFile, "client")

	other := sha256.Sum256([]byte("some other key"))
	w := newMutualTLSWorker(server.URL, &config.ProbeTLSConfig{
		ClientCertFile:   certFile,
		ClientKeyFile:    keyFile,
		ServerSPKISHA256: []string{base64.StdEncoding.EncodeToString(other[:])},
	})

	result := w.runProbe()
	if result.success {
		t.Fatal("probe succeeded against a server whose key is not pinned")
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "is not pinned") {
		t.Fatalf("got error %v, want one saying the server key is not pinned", result.err)
	}
	if got := clients(); len(got) != 0 {
		t.Fatalf("server served %v, want no requests", got)
	}
}

// A client certificate that cannot be read fails the probe instead of
// connecting without one.
func TestProbeFailsWithoutItsClientCertificate(t *testing.T) {
	ca := newTestCA(t)
	server, clients := newMutualTLSEndpoint(t, ca)

	dir := t.TempDir()
	w := newMutualTLSWorker(server.URL, &config.ProbeTLSConfig{
		ClientCertFile:   filepath.Join(dir, "missing.crt"),
		ClientKeyFile:    filepath.Join(dir, "missing.key"),
		ServerSPKISHA256: []string{spkiPin(server.Certificate())},
	})

	result := w.runProbe()
	if result.success {
		t.Fatal("probe succeeded without its client certificate")
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "loading the probe client certificate") {
		t.Fatalf("got error %v, want one about loading the client certificate", result.err)
	}
	if got := clients(); len(got) != 0 {
		t.Fatalf("server served %v, want no requests", got)
	}
}

// A pinned server that redirects to plain http must not get the probe resent
// there: the hop would skip TLS, carry the request body and API key in
// cleartext, and its unauthenticated answer could pass the probe.
func TestProbeRefusesARedirectToPlainHTTP(t *testing.T) {
	var mu sync.Mutex
	var plainRequests []string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		plainRequests = append(plainRequests, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "OK"}}},
		})
	}))
	t.Cleanup(plain.Close)

	ca := newTestCA(t)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	pinned := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	pinned.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	pinned.StartTLS()
	t.Cleanup(pinned.Close)

	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	ca.writeClientCert(t, certFile, keyFile, "client")

	w := newMutualTLSWorker(pinned.URL, &config.ProbeTLSConfig{
		ClientCertFile:   certFile,
		ClientKeyFile:    keyFile,
		ServerSPKISHA256: []string{spkiPin(pinned.Certificate())},
	})
	w.endpoint.APIKey = "test-key"

	result := w.runProbe()
	if result.success {
		t.Error("probe succeeded through a redirect to plain http")
	}
	if result.err == nil || !strings.Contains(result.err.Error(), "non-HTTPS") {
		t.Errorf("got error %v, want one refusing the non-HTTPS redirect", result.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(plainRequests) != 0 {
		t.Errorf("plain http server received %v, want nothing", plainRequests)
	}
}
