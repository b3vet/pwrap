package keys

import (
	"errors"
	"testing"
)

func TestHashVerify_RoundTrip(t *testing.T) {
	encoded, err := Hash("hunter2")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := Verify("hunter2", encoded); err != nil {
		t.Fatalf("verify correct: %v", err)
	}
	if err := Verify("nope", encoded); !errors.Is(err, ErrMismatch) {
		t.Fatalf("verify wrong: got %v, want ErrMismatch", err)
	}
}

func TestHash_UniquePerCall(t *testing.T) {
	a, _ := Hash("same-input")
	b, _ := Hash("same-input")
	if a == b {
		t.Fatalf("salt not randomised: %q == %q", a, b)
	}
}
