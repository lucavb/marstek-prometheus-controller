package promclient_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lucavb/marstek-prometheus-controller/internal/promclient"
)

func TestGridPowerReader_FallbackDisabled(t *testing.T) {
	r := promclient.NewGridPowerReader("http://example", "primary", "", 0)

	if r.FallbackEnabled() {
		t.Fatal("FallbackEnabled() = true, want false")
	}

	_, err := r.QueryFallback(context.Background())
	if !errors.Is(err, promclient.ErrEmptyResult) {
		t.Fatalf("QueryFallback() error = %v, want ErrEmptyResult", err)
	}
}

func TestGridPowerReader_FallbackEnabled(t *testing.T) {
	r := promclient.NewGridPowerReader("http://example", "primary", "fallback", 0)

	if !r.FallbackEnabled() {
		t.Fatal("FallbackEnabled() = false, want true")
	}
}
