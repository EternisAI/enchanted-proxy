package probe

import (
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
func newProbeHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = config.DefaultProbeTimeout
	}

	// Never let connection setup alone consume the whole budget.
	connect := min(connectTimeout, timeout)

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: connect,
			}).DialContext,
			TLSHandshakeTimeout:   connect,
			ResponseHeaderTimeout: timeout,
			DisableKeepAlives:     true,
		},
	}
}
