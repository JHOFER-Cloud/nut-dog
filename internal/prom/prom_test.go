package prom

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serve returns a Client pointed at a test server that replies with the given
// status code and body for /api/v1/query, plus the query it received.
func serve(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, time.Second), &gotQuery
}

func TestTruthy(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    bool
		wantErr bool
	}{
		{
			name: "vector with active mode is truthy",
			body: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"mode":"shed"},"value":[1783808562.7,"1"]}]}}`,
			want: true,
		},
		{
			name: "empty vector is false",
			body: `{"status":"success","data":{"resultType":"vector","result":[]}}`,
			want: false,
		},
		{
			name: "vector present but zero is false",
			body: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"mode":"shed"},"value":[1783808562.7,"0"]}]}}`,
			want: false,
		},
		{
			name: "any non-zero sample is truthy",
			body: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"mode":"running"},"value":[1,"0"]},{"metric":{"mode":"shed"},"value":[1,"1"]}]}}`,
			want: true,
		},
		{
			name: "non-zero scalar is truthy",
			body: `{"status":"success","data":{"resultType":"scalar","result":[1783808562.7,"1"]}}`,
			want: true,
		},
		{
			name: "zero scalar is false",
			body: `{"status":"success","data":{"resultType":"scalar","result":[1783808562.7,"0"]}}`,
			want: false,
		},
		{
			name:    "error status is an error",
			body:    `{"status":"error","errorType":"bad_data","error":"parse error"}`,
			wantErr: true,
		},
		{
			name:    "matrix result is unsupported",
			body:    `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
			wantErr: true,
		},
		{
			name:    "malformed vector sample errors (fail-closed)",
			body:    `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1783808562.7]}]}}`,
			wantErr: true,
		},
		{
			name:    "non-numeric vector value errors (fail-closed)",
			body:    `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"nope"]}]}}`,
			wantErr: true,
		},
		{
			name:    "malformed scalar result errors (fail-closed)",
			body:    `{"status":"success","data":{"resultType":"scalar","result":[1783808562.7]}}`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, _ := serve(t, http.StatusOK, c.body)
			got, err := client.Truthy("q")
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, c.wantErr)
			}
			if !c.wantErr && got != c.want {
				t.Errorf("Truthy = %v, want %v", got, c.want)
			}
		})
	}
}

// A non-200 HTTP response is an error (callers fail-closed on it).
func TestTruthyHTTPError(t *testing.T) {
	client, _ := serve(t, http.StatusServiceUnavailable, "unavailable")
	if _, err := client.Truthy("q"); err == nil {
		t.Fatal("expected error on 503, got nil")
	}
}

// A dead endpoint (connection refused) is an error, not a false. Bind a real
// server to claim a port, then close it so the address is reliably unreachable —
// avoids assuming any fixed port is free.
func TestTruthyUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	client := NewClient(url, 200*time.Millisecond)
	if _, err := client.Truthy("q"); err == nil {
		t.Fatal("expected error on unreachable endpoint, got nil")
	}
}

func TestTruthyEscapesQuery(t *testing.T) {
	client, got := serve(t, http.StatusOK,
		`{"status":"success","data":{"resultType":"vector","result":[]}}`)
	q := `energy_watchdog_mode{mode="shed"} == 1`
	if _, err := client.Truthy(q); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *got != q {
		t.Errorf("server received query %q, want %q", *got, q)
	}
}
