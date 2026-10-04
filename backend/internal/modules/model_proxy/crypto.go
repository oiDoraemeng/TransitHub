package model_proxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

var errEncryptionKeyUnavailable = errors.New("PROXY_KEY_ENCRYPTION_KEY is not configured")

type KeyCipher struct {
	aead cipher.AEAD
}

func NewKeyCipher(encoded string) (*KeyCipher, error) {
	if strings.TrimSpace(encoded) == "" {
		return &KeyCipher{}, nil
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("PROXY_KEY_ENCRYPTION_KEY must be base64-encoded 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &KeyCipher{aead: aead}, nil
}

func (c *KeyCipher) Available() bool { return c != nil && c.aead != nil }

func (c *KeyCipher) Encrypt(value string) (string, error) {
	if !c.Available() {
		return "", errEncryptionKeyUnavailable
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nil, nonce, []byte(value), nil)
	return base64.RawStdEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func (c *KeyCipher) Decrypt(value string) (string, error) {
	if !c.Available() {
		return "", errEncryptionKeyUnavailable
	}
	data, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(data) < c.aead.NonceSize() {
		return "", errors.New("invalid encrypted proxy key")
	}
	plain, err := c.aead.Open(nil, data[:c.aead.NonceSize()], data[c.aead.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func hashKey(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}

func generateEntryKey(ownerType string) (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	prefix := "sk-th-"
	if ownerType == OwnerSmartGroup {
		prefix = "sk-thg-"
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func keyPreview(key string) string {
	if len(key) <= 14 {
		return key
	}
	return key[:9] + "..." + key[len(key)-4:]
}
