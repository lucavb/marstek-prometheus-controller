package promclient

import (
	"context"
	"time"
)

// GridPowerReader queries a primary grid-power PromQL expression and an
// optional fallback expression (e.g. smart-plug sum) against the same
// Prometheus base URL.
type GridPowerReader struct {
	primary  *Client
	fallback *Client
}

// NewGridPowerReader creates a GridPowerReader. fallbackQuery may be empty to
// disable the fallback client.
func NewGridPowerReader(baseURL, primaryQuery, fallbackQuery string, timeout time.Duration) *GridPowerReader {
	r := &GridPowerReader{
		primary: New(baseURL, primaryQuery, timeout),
	}
	if fallbackQuery != "" {
		r.fallback = New(baseURL, fallbackQuery, timeout)
	}
	return r
}

// FallbackEnabled reports whether a fallback query is configured.
func (r *GridPowerReader) FallbackEnabled() bool {
	return r.fallback != nil
}

// QueryPrimary executes the primary grid-power instant query.
func (r *GridPowerReader) QueryPrimary(ctx context.Context) (Sample, error) {
	return r.primary.Query(ctx)
}

// QueryFallback executes the fallback instant query. Returns ErrEmptyResult
// when no fallback query is configured.
func (r *GridPowerReader) QueryFallback(ctx context.Context) (Sample, error) {
	if r.fallback == nil {
		return Sample{}, ErrEmptyResult
	}
	return r.fallback.Query(ctx)
}
