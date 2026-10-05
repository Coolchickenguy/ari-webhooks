package ids

import (
	"crypto/rand"

	"github.com/nrednav/cuid2"
)

func Cuid() string {
	return cuid2.Generate()
}

// ShipId mirrors ari's webhook.ts shipId(): 8 random chars from a-z0-9, rejection-sampled.
func ShipId() string {
	out := make([]byte, 0, 8)
	for len(out) < 8 {
		buf := make([]byte, 16)
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		for _, b := range buf {
			if b >= 252 { // 252 = 36 * 7; reject so each of the 36 chars is equiprobable
				continue
			}
			out = append(out, "abcdefghijklmnopqrstuvwxyz0123456789"[b%36])
			if len(out) == 8 {
				break
			}
		}
	}
	return string(out)
}
