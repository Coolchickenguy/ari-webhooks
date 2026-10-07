package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

const (
	SignatureHeader  = "x-ari-signature"
	TimestampHeader  = "x-ari-timestamp"
	DeliveryIdHeader = "x-ari-delivery-id"
)

// VerifyInbound checks the legacy ingest HMAC: hex SHA-256 of the raw body alone, constant-time. // authz: this is the only credential on ingest
func VerifyInbound(secret string, rawBody []byte, header string) bool {
	return VerifyInboundAt(secret, rawBody, header, "", time.Time{})
}

// VerifyInboundAt checks the ingest HMAC. Without a timestamp header it is the
// legacy body-only scheme; with one, the timestamp must be integer unix seconds
// close to now and the HMAC covers "{unixSeconds}.{rawBody}", so a captured
// request cannot be replayed later. // authz: this is the only credential on ingest
func VerifyInboundAt(secret string, rawBody []byte, signatureHeader, timestampHeader string, now time.Time) bool {
	if signatureHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	if timestampHeader != "" {
		ts, err := strconv.ParseInt(strings.TrimSpace(timestampHeader), 10, 64)
		if err != nil {
			return false
		}
		if skew := now.Unix() - ts; skew > 300 || skew < -300 { // 5-minute replay window, either direction so a slightly fast sender clock still verifies
			return false
		}
		mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	}
	mac.Write(rawBody)
	expected := hex.EncodeToString(mac.Sum(nil))
	got := strings.ToLower(strings.TrimSpace(signatureHeader)) // ari accepts case-insensitive hex with surrounding whitespace
	return hmac.Equal([]byte(got), []byte(expected))
}

// SignOutbound covers "{unixSeconds}.{deliveryId}.{rawBody}", matching ari's outbound.ts scheme.
func SignOutbound(secret, deliveryId string, unixSeconds int64, rawBody []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(unixSeconds, 10) + "." + deliveryId + "."))
	mac.Write(rawBody)
	return hex.EncodeToString(mac.Sum(nil))
}
