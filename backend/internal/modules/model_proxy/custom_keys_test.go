package model_proxy

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeSmartGroupCustomKeys(t *testing.T) {
	got, err := normalizeSmartGroupCustomKeys([]string{" custom-a ", "custom-a", "custom-b"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"custom-a", "custom-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keys=%v want=%v", got, want)
	}
}

func TestNormalizeSmartGroupCustomKeysRejectsWhitespace(t *testing.T) {
	if _, err := normalizeSmartGroupCustomKeys([]string{"custom key"}); err == nil {
		t.Fatal("expected whitespace validation error")
	}
}

func TestNormalizeSmartGroupCustomKeysLimitsCount(t *testing.T) {
	values := make([]string, maxSmartGroupCustomKeys+1)
	for index := range values {
		values[index] = "key-" + strings.Repeat("x", index+1)
	}
	if _, err := normalizeSmartGroupCustomKeys(values); err == nil {
		t.Fatal("expected custom key count validation error")
	}
}
