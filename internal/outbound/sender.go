package outbound

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hackclub/ari-webhooks/internal/signature"
	"github.com/hackclub/ari-webhooks/internal/ssrf"
)

var errRedirectBlocked = errors.New("redirect blocked")

func newClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   10 * time.Second, // a hung connection must not dangle the retry loop
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return errRedirectBlocked // a public host must not 30x into the internal network
		},
	}
}

var (
	httpClient               = newClient(ssrf.Transport()) // every send is re-vetted at dial time, so a destination that rebinds to an internal address after the URL check still gets no connection
	privateDestinationClient = newClient(http.DefaultTransport)
)

var whitespaceRun = regexp.MustCompile(`\s+`)

type attemptResult struct {
	delivered   bool
	httpStatus  *int
	errorDetail string
}

func sendOnce(ctx context.Context, client *http.Client, deliveryId, url, secret string, rawBody []byte) attemptResult {
	timestamp := time.Now().Unix()
	sig := signature.SignOutbound(secret, deliveryId, timestamp, rawBody)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(rawBody)))
	if err != nil {
		return attemptResult{errorDetail: describeSendError(err)}
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set(signature.SignatureHeader, sig)
	req.Header.Set(signature.TimestampHeader, strconv.FormatInt(timestamp, 10))
	req.Header.Set(signature.DeliveryIdHeader, deliveryId)

	res, err := client.Do(req)
	if err != nil {
		return attemptResult{errorDetail: describeSendError(err)}
	}
	defer res.Body.Close()
	bodyRaw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10)) // enough for the audit snippet
	if res.StatusCode >= 200 && res.StatusCode <= 299 {
		return attemptResult{delivered: true, httpStatus: &res.StatusCode}
	}
	detail := "http " + strconv.Itoa(res.StatusCode)
	snippet := sliceChars(strings.TrimSpace(whitespaceRun.ReplaceAllString(string(bodyRaw), " ")), 300)
	if snippet != "" {
		detail += " - " + snippet
	}
	return attemptResult{httpStatus: &res.StatusCode, errorDetail: detail}
}

func describeSendError(err error) string {
	if errors.Is(err, errRedirectBlocked) {
		return "redirect blocked (destination tried to redirect)"
	}
	if errors.Is(err, ssrf.ErrUnsafeAddress) {
		return "destination resolves to a private or reserved address"
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout after 10s (no response from destination)"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		if dnsErr.IsNotFound {
			return "DNS lookup failed - host not found (ENOTFOUND)"
		}
		return "DNS lookup timed out (resolver unreachable) (EAI_AGAIN)"
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused (nothing listening / port closed) (ECONNREFUSED)"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset by peer (ECONNRESET)"
	case errors.Is(err, syscall.ETIMEDOUT):
		return "connection timed out (no route / firewall drop) (ETIMEDOUT)"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "host unreachable (EHOSTUNREACH)"
	case errors.Is(err, syscall.ENETUNREACH):
		return "network unreachable (ENETUNREACH)"
	case errors.Is(err, syscall.EPIPE):
		return "connection closed mid-request (EPIPE)"
	}
	var certErr *x509.CertificateInvalidError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	switch {
	case errors.As(err, &certErr):
		if certErr.Reason == x509.Expired {
			return "TLS certificate expired (CERT_HAS_EXPIRED)"
		}
		return fmt.Sprintf("invalid TLS certificate: %v", certErr)
	case errors.As(err, &unknownAuthority):
		return "untrusted TLS certificate (unknown CA) (UNABLE_TO_VERIFY_LEAF_SIGNATURE)"
	case errors.As(err, &hostnameErr):
		return "TLS certificate hostname mismatch (ERR_TLS_CERT_ALTNAME_INVALID)"
	}
	var urlErr interface{ Unwrap() error }
	if errors.As(err, &urlErr) && urlErr.Unwrap() != nil {
		return urlErr.Unwrap().Error()
	}
	return err.Error()
}

func sliceChars(s string, max int) string {
	r := []rune(s)
	if len(r) > max {
		return string(r[:max])
	}
	return s
}
