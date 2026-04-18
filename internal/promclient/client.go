// Package promclient provides a minimal Prometheus HTTP API client that reads
// a single instant-query metric value (a scalar float64 + sample timestamp).
package promclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Sample is a single instant-query result.
type Sample struct {
	Watts      float64
	SampleTime time.Time
}

// Client queries a Prometheus instance for a single scalar metric.
type Client struct {
	baseURL    string
	query      string
	httpClient *http.Client
}

// New creates a Client. baseURL is the Prometheus base URL (e.g.
// "http://prometheus:9090"). query is the PromQL expression that must
// return exactly one time-series with a numeric value.
func New(baseURL, query string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		query:   query,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// Query executes an instant query and returns the first sample. It returns an
// error if the HTTP request fails, the response cannot be parsed, or the result
// set is empty.
func (c *Client) Query(ctx context.Context) (Sample, error) {
	u, err := url.Parse(c.baseURL + "/api/v1/query")
	if err != nil {
		return Sample{}, fmt.Errorf("promclient: invalid base URL: %w", err)
	}
	q := u.Query()
	q.Set("query", c.query)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Sample{}, fmt.Errorf("promclient: build request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Sample{}, fmt.Errorf("promclient: http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Sample{}, fmt.Errorf("promclient: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Sample{}, fmt.Errorf("promclient: http %d: %s", resp.StatusCode, string(body))
	}

	return parseResponse(body)
}

// apiResponse is the envelope returned by /api/v1/query.
type apiResponse struct {
	Status string      `json:"status"`
	Data   apiData     `json:"data"`
	Error  string      `json:"error"`
}

type apiData struct {
	ResultType string      `json:"resultType"`
	Result     []apiResult `json:"result"`
}

type apiResult struct {
	Metric map[string]string `json:"metric"`
	Value  [2]json.RawMessage `json:"value"` // [timestamp, value]
}

func parseResponse(body []byte) (Sample, error) {
	var r apiResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Sample{}, fmt.Errorf("promclient: parse: %w", err)
	}
	if r.Status != "success" {
		return Sample{}, fmt.Errorf("promclient: prometheus error: %s", r.Error)
	}
	if len(r.Data.Result) == 0 {
		return Sample{}, fmt.Errorf("promclient: empty result set")
	}

	res := r.Data.Result[0]

	// res.Value[0] is the Unix timestamp (float in JSON), res.Value[1] is the
	// string-encoded sample value (Prometheus always quotes numeric values).
	var tsFloat float64
	if err := json.Unmarshal(res.Value[0], &tsFloat); err != nil {
		return Sample{}, fmt.Errorf("promclient: parse timestamp: %w", err)
	}

	var valStr string
	if err := json.Unmarshal(res.Value[1], &valStr); err != nil {
		return Sample{}, fmt.Errorf("promclient: parse value string: %w", err)
	}
	watts, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return Sample{}, fmt.Errorf("promclient: parse value %q: %w", valStr, err)
	}

	sec := int64(tsFloat)
	nsec := int64((tsFloat - float64(sec)) * 1e9)
	return Sample{
		Watts:      watts,
		SampleTime: time.Unix(sec, nsec),
	}, nil
}
