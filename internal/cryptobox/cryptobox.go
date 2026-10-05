package cryptobox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

type Codec struct {
	aead cipher.AEAD
}

func New(key []byte) (*Codec, error) {
	if len(key) != 32 { // every secret at rest depends on this key length being exact
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Codec{aead: aead}, nil
}

// Encrypt returns iv.tag.ciphertext, each standard base64, matching ari's crypto.ts blobs.
func (c *Codec) Encrypt(plain string) (string, error) {
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nil, iv, []byte(plain), nil)
	ct := sealed[:len(sealed)-16]
	tag := sealed[len(sealed)-16:]
	enc := base64.StdEncoding.EncodeToString
	return enc(iv) + "." + enc(tag) + "." + enc(ct), nil
}

func (c *Codec) Decrypt(blob string) (string, error) {
	parts := strings.Split(blob, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("encrypted blob must have 3 dot-separated parts, got %d", len(parts))
	}
	iv, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("bad iv encoding: %w", err)
	}
	tag, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("bad tag encoding: %w", err)
	}
	ct, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("bad ciphertext encoding: %w", err)
	}
	if len(iv) != c.aead.NonceSize() { // aead.Open panics, not errors, on a wrong-size nonce
		return "", fmt.Errorf("iv must be %d bytes, got %d", c.aead.NonceSize(), len(iv))
	}
	plain, err := c.aead.Open(nil, iv, append(ct, tag...), nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
