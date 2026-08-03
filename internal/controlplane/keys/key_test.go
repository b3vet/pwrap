package keys

import (
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	pt, prefix, err := Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !strings.HasPrefix(pt, KeyPrefix) {
		t.Fatalf("plaintext missing prefix: %q", pt)
	}
	if len(prefix) != PrefixLookup {
		t.Fatalf("prefix len = %d, want %d", len(prefix), PrefixLookup)
	}
	parsed, err := ParsePrefix(pt)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed != prefix {
		t.Fatalf("parsed prefix %q != generated %q", parsed, prefix)
	}
}

func TestParsePrefix_Errors(t *testing.T) {
	if _, err := ParsePrefix("nope"); err == nil {
		t.Fatal("expected error for missing prefix")
	}
	if _, err := ParsePrefix(KeyPrefix + "x"); err == nil {
		t.Fatal("expected error for short payload")
	}
}
