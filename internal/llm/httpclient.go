package llm

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// newHTTPClient builds the client used for provider traffic.
//
// The redirect policy is the reason this exists: net/http strips
// "Authorization" when a redirect leaves the initial host, but it does not know
// about "x-api-key" (the Anthropic header), so that credential would be replayed
// to whatever host the response names. Every provider credential is scoped to
// the endpoint the user configured, so a redirect to another host is refused
// outright rather than followed without the header — a model API has no
// legitimate need for one.
//
// A redirect within the same host (a trailing-slash fix, say) is followed
// normally, credentials intact.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: sameHostOnly,
	}
}

// sameHostOnly allows a redirect only when it stays on the host the request
// started from. Host includes the port, so a port change counts as leaving: the
// credential is bound to the configured endpoint, and a different port is a
// different endpoint even when the name resolves to the same machine.
func sameHostOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return errors.New("refusing to follow a redirect from " + via[0].URL.Host + " to another host: " + req.URL.Host)
	}
	return nil
}
