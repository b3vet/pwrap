package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestInit_NoExportersStillReturnsProviders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	p, err := Init(ctx, Options{ServiceName: "test"})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if p == nil || p.Tracer == nil || p.Meter == nil {
		t.Fatal("expected non-nil providers")
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestInit_PrometheusOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Use port 0 to avoid clashes in CI. The promhttp server binding is best-effort
	// in this test — what matters is that Init doesn't error and Shutdown returns.
	p, err := Init(ctx, Options{ServiceName: "test", PrometheusAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestStripScheme(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:4318": "localhost:4318",
		"https://otel.acme.com": "otel.acme.com",
		"localhost:4318":        "localhost:4318",
		"":                      "",
	} {
		if got := stripScheme(in); got != want {
			t.Errorf("stripScheme(%q) = %q, want %q", in, got, want)
		}
	}
}
