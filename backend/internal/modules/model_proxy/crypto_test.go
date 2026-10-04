package model_proxy

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestKeyCipherRoundTrip(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))
	cipher, err := NewKeyCipher(encoded)
	if err != nil {
		t.Fatalf("NewKeyCipher: %v", err)
	}
	encrypted, err := cipher.Encrypt("sk-th-secret")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if strings.Contains(encrypted, "sk-th-secret") {
		t.Fatal("ciphertext contains plaintext")
	}
	plain, err := cipher.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if plain != "sk-th-secret" {
		t.Fatalf("Decrypt = %q", plain)
	}
}

func TestKeyCipherRequiresExplicitKey(t *testing.T) {
	cipher, err := NewKeyCipher("")
	if err != nil {
		t.Fatal(err)
	}
	if cipher.Available() {
		t.Fatal("empty encryption key must not enable encryption")
	}
	if _, err := cipher.Encrypt("secret"); err == nil {
		t.Fatal("Encrypt should fail closed")
	}
}

func TestGeneratedEntryKeyPrefixes(t *testing.T) {
	routeKey, err := generateEntryKey(OwnerRoute)
	if err != nil || !strings.HasPrefix(routeKey, "sk-th-") {
		t.Fatalf("route key = %q, err=%v", routeKey, err)
	}
	groupKey, err := generateEntryKey(OwnerSmartGroup)
	if err != nil || !strings.HasPrefix(groupKey, "sk-thg-") {
		t.Fatalf("group key = %q, err=%v", groupKey, err)
	}
	if hashKey(routeKey) == hashKey(groupKey) {
		t.Fatal("different keys have the same hash")
	}
}
