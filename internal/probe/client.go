package probe

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/eternisai/enchanted-proxy/internal/config"
)

// connectTimeout bounds the connection setup phases (TCP dial, TLS handshake).
// It is a share of the overall probe budget rather than the whole of it, so a
// probe that spends its time connecting still leaves room for a response.
const connectTimeout = 10 * time.Second

// newProbeHTTPClient builds the HTTP client for one probe worker.
//
// The timeout is the total budget for a probe request, and it is also the
// response-header timeout. A probe is a non-streaming completion, so the
// upstream sends no headers until the model has finished generating: a header
// timeout shorter than the overall budget would be the only limit that ever
// applied, and would fail slow-but-working endpoints while the rest of the
// budget went unused.
//
// A non-positive timeout selects the default, which keeps workers built outside
// the config path (tests) on the same budget as configured ones.
//
// A non-nil tlsCfg adds the endpoint's mutual TLS (see pinnedTLSConfig).
func newProbeHTTPClient(timeout time.Duration, tlsCfg *config.ProbeTLSConfig) *http.Client {
	if timeout <= 0 {
		timeout = config.DefaultProbeTimeout
	}

	// Never let connection setup alone consume the whole budget.
	connect := min(connectTimeout, timeout)

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: connect,
		}).DialContext,
		TLSHandshakeTimeout:   connect,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	if tlsCfg != nil {
		transport.TLSClientConfig = pinnedTLSConfig(tlsCfg)
		// A redirect to plain http would resend the request, with its API key,
		// without TLS, and its answer could pass the probe. An https redirect
		// is still held to the pinned key.
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
			}
			if len(via) >= 10 { // the default client's limit
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
	}
	return client
}

// pinnedTLSConfig authenticates the prober with the configured client
// certificate and accepts only a server whose public key is pinned.
//
// The server certificate is self-signed, so the usual chain verification is
// off and VerifyConnection, which runs regardless, checks the pin instead.
// The client certificate is read on every handshake (keep-alives are off, so
// that is every probe): it is rotated on disk, and a stale copy would expire.
func pinnedTLSConfig(cfg *config.ProbeTLSConfig) *tls.Config {
	// The config validation checked that every pin decodes to 32 bytes.
	pins := make(map[[sha256.Size]byte]bool, len(cfg.ServerSPKISHA256))
	for _, pin := range cfg.ServerSPKISHA256 {
		var sum [sha256.Size]byte
		if b, err := base64.StdEncoding.DecodeString(pin); err == nil && len(b) == sha256.Size {
			copy(sum[:], b)
			pins[sum] = true
		}
	}

	return &tls.Config{
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server presented no certificate")
			}
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if !pins[sum] {
				return fmt.Errorf("server public key %s is not pinned", base64.StdEncoding.EncodeToString(sum[:]))
			}
			return nil
		},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
			if err != nil {
				return nil, fmt.Errorf("loading the probe client certificate: %w", err)
			}
			return &cert, nil
		},
	}
}
