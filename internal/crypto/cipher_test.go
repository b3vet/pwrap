package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func newCipher(t *testing.T) Cipher {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	c, err := NewFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRoundTrip(t *testing.T) {
	c := newCipher(t)
	cases := []string{"", "hello", "a longer password with spaces and digits 12345!@#"}
	for _, in := range cases {
		ct, err := c.Encrypt(in)
		if err != nil {
			t.Fatalf("encrypt(%q): %v", in, err)
		}
		if in == "" {
			if ct != "" {
				t.Fatalf("empty input should produce empty output, got %q", ct)
			}
			continue
		}
		if !strings.HasPrefix(ct, "v1:") {
			t.Fatalf("expected v1: prefix, got %q", ct)
		}
		pt, err := c.Decrypt(ct)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if pt != in {
			t.Fatalf("roundtrip mismatch: got %q want %q", pt, in)
		}
	}
}

func TestDecrypt_PassThroughPlaintext(t *testing.T) {
	c := newCipher(t)
	got, err := c.Decrypt("legacy-cleartext-value")
	if err != nil {
		t.Fatal(err)
	}
	if got != "legacy-cleartext-value" {
		t.Fatalf("got %q", got)
	}
}

func TestNoop_RefusesEncryptedInput(t *testing.T) {
	enc := newCipher(t)
	ct, _ := enc.Encrypt("secret")

	n := NewNoop()
	if _, err := n.Decrypt(ct); err == nil {
		t.Fatal("noop should error on v1: input")
	}
	// Legacy plaintext passes.
	if v, err := n.Decrypt("plain"); err != nil || v != "plain" {
		t.Fatalf("got %q %v", v, err)
	}
}

func TestNewFromBase64Key(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(key)
	c, err := NewFromBase64Key(b64)
	if err != nil {
		t.Fatal(err)
	}
	ct, _ := c.Encrypt("x")
	pt, err := c.Decrypt(ct)
	if err != nil || pt != "x" {
		t.Fatalf("roundtrip: got %q err=%v", pt, err)
	}

	if _, err := NewFromBase64Key(""); err == nil {
		t.Fatal("empty key should error")
	}
	if _, err := NewFromBase64Key("not-base64-$$$"); err == nil {
		t.Fatal("bad base64 should error")
	}
}

func TestIsEnvelope(t *testing.T) {
	if !IsEnvelope("v1:abc") {
		t.Fatal("v1: should be envelope")
	}
	if IsEnvelope("plain") {
		t.Fatal("plain should not be envelope")
	}
}

func TestTamperedCiphertext_FailsGCM(t *testing.T) {
	c := newCipher(t)
	ct, _ := c.Encrypt("hello")
	// Flip a single character in the base64 body.
	body := ct[len("v1:"):]
	tampered := "v1:" + flipFirstAlnum(body)
	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatal("expected GCM auth failure")
	}
}

func flipFirstAlnum(s string) string {
	b := []byte(s)
	for i, r := range b {
		if r >= 'A' && r <= 'Y' {
			b[i] = r + 1
			return string(b)
		}
		if r >= 'a' && r <= 'y' {
			b[i] = r + 1
			return string(b)
		}
		if r >= '0' && r <= '8' {
			b[i] = r + 1
			return string(b)
		}
	}
	return s
}
