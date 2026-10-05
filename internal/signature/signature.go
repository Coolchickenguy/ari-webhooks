package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

const (
	SignatureHeader  = "x-ari-signature"
	TimestampHeader  = "x-ari-timestamp"
	DeliveryIdHeader = "x-ari-delivery-id"
)

// VerifyInbound checks the ingest HMAC: hex SHA-256 of the raw body, constant-time. // authz: this is the only credential on ingest
func VerifyInbound(secret string, rawBody []byte, header string) bool {
	if header == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	expected := hex.EncodeToString(mac.Sum(nil))
	got := strings.ToLower(strings.TrimSpace(header)) // ari accepts case-insensitive hex with surrounding whitespace
	return hmac.Equal([]byte(got), []byte(expected))
}

// SignOutbound covers "{unixSeconds}.{deliveryId}.{rawBody}", matching ari's outbound.ts scheme.
func SignOutbound(secret, deliveryId string, unixSeconds int64, rawBody []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(unixSeconds, 10) + "." + deliveryId + "."))
	mac.Write(rawBody)
	return hex.EncodeToString(mac.Sum(nil))
}
