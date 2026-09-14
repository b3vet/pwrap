// Command refreshcheck proves the Go SDK re-exchanges credentials before the
// server expires them. Driven by scripts/refresh-check.sh; nightly only,
// because it has to outlive a credential to prove anything.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/b3vet/pwrap/sdk/go/pwrap"
)

func main() {
	raw, err := os.ReadFile("/tmp/refresh-key.txt")
	if err != nil {
		fmt.Fprintln(os.Stderr, "read key:", err)
		os.Exit(1)
	}
	wait := 105
	if v, err := strconv.Atoi(os.Getenv("PWRAP_REFRESH_WAIT")); err == nil && v > 0 {
		wait = v
	}

	ctx := context.Background()
	c, err := pwrap.New(ctx, pwrap.Config{
		ControlURL: "http://localhost:8080",
		APIKey:     strings.TrimSpace(string(raw)),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer c.Close()

	first := c.ExpiresAt()
	// Deliberately taken before any refresh: the swap has to reach handles that
	// already exist, not only ones created afterwards.
	notes := c.Table("refreshcheck")
	if _, err := notes.Insert(ctx, map[string]any{"n": 0}); err != nil {
		fmt.Fprintln(os.Stderr, "first insert:", err)
		os.Exit(1)
	}
	time.Sleep(time.Duration(wait) * time.Second)

	if _, err := notes.Insert(ctx, map[string]any{"n": 1}); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: insert after %ds: %v\n", wait, err)
		os.Exit(1)
	}
	if !c.ExpiresAt().After(first) {
		fmt.Fprintf(os.Stderr, "FAIL: expiry never advanced from %s\n", first.Format(time.RFC3339))
		os.Exit(1)
	}
	fmt.Printf("  go: PASS (expiry %s -> %s)\n",
		first.Format(time.RFC3339), c.ExpiresAt().Format(time.RFC3339))
}
