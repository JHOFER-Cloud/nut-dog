// Package prom runs instant PromQL queries against Prometheus' HTTP API. nut-dog
// uses it for one thing only: reading another controller's state — specifically
// whether an external authority (energy-watchdog) is deliberately holding a load
// powered off — so nut-dog can defer a wake instead of fighting it. It is a
// read-only client, the query counterpart to internal/nut.
package prom

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client queries one Prometheus HTTP API base URL.
type Client struct {
	base string
	http *http.Client
}

// NewClient builds a client for base (e.g. "http://prometheus:9090"). timeout
// bounds each query.
func NewClient(base string, timeout time.Duration) *Client {
	return &Client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{Timeout: timeout},
	}
}

// queryResponse is the subset of /api/v1/query we read.
type queryResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// Truthy runs an instant query and reports whether its result is "on": a vector
// with at least one non-zero sample, or a non-zero scalar. An empty vector is
// false. A non-success HTTP response, an error status, or a body we can't parse
// is returned as an error so the caller can pick its fail-safe.
func (c *Client) Truthy(query string) (bool, error) {
	resp, err := c.http.Get(c.base + "/api/v1/query?query=" + url.QueryEscape(query))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, err
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("prometheus %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var qr queryResponse
	if err := json.Unmarshal(body, &qr); err != nil {
		return false, fmt.Errorf("decode prometheus response: %w", err)
	}
	if qr.Status != "success" {
		return false, fmt.Errorf("prometheus query %q: %s", query, qr.Error)
	}
	switch qr.Data.ResultType {
	case "vector":
		return vectorTruthy(qr.Data.Result)
	case "scalar":
		return scalarTruthy(qr.Data.Result)
	default:
		// matrix/string don't come from an instant boolean gate; treat as a config
		// mistake rather than silently reading it as false.
		return false, fmt.Errorf("unsupported result type %q (use an instant query)", qr.Data.ResultType)
	}
}

// vectorTruthy reports whether any sample in an instant-vector result has a
// non-zero value. Each element is {metric, value:[ts, "<val>"]}. A sample we
// can't parse is an error, not a silent false — the caller fails closed on it
// rather than accidentally allowing a wake off an unparseable response.
func vectorTruthy(raw json.RawMessage) (bool, error) {
	var samples []struct {
		Value []json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &samples); err != nil {
		return false, fmt.Errorf("decode vector result: %w", err)
	}
	for _, s := range samples {
		if len(s.Value) != 2 {
			return false, fmt.Errorf("malformed vector sample: want [ts, value], got %d fields", len(s.Value))
		}
		f, ok := sampleFloat(s.Value[1])
		if !ok {
			return false, fmt.Errorf("vector sample value not numeric: %s", s.Value[1])
		}
		if f != 0 {
			return true, nil
		}
	}
	return false, nil
}

// scalarTruthy reports whether a scalar result [ts, "<val>"] is non-zero. A
// result that isn't the expected 2-element shape is an error (fail-closed), not
// a silent false.
func scalarTruthy(raw json.RawMessage) (bool, error) {
	var v []json.RawMessage
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, fmt.Errorf("decode scalar result: %w", err)
	}
	if len(v) != 2 {
		return false, fmt.Errorf("malformed scalar result: want [ts, value], got %d fields", len(v))
	}
	f, ok := sampleFloat(v[1])
	if !ok {
		return false, fmt.Errorf("scalar value not numeric: %s", v[1])
	}
	return f != 0, nil
}

// sampleFloat parses a Prometheus sample value, which the API encodes as a JSON
// string ("1", "+Inf", ...).
func sampleFloat(raw json.RawMessage) (float64, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
