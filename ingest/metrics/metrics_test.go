package metrics

import (
	"strings"
	"testing"
)

func TestExposition(t *testing.T) {
	r := NewRegistry("la_ingest_")
	req := r.Requests("requests_total", "Requests by status code and format.")
	c := r.Counter("bytes_in_total", "Body bytes accepted.")
	h := r.Histogram("fsync_seconds", "fsync duration.", 0.001, 0.01, 0.1)
	r.Gauge("wal_local_bytes", "Local WAL bytes.", func() float64 { return 12345 })
	r.Gauge("tokens_age_seconds", "Age.", func() float64 { return 1.5 })

	req.Inc(FormatNative, 200)
	req.Inc(FormatNative, 200)
	req.Inc(FormatPostHog, 401)
	req.Inc(99, 9999) // out of range values are folded, not dropped or panicking
	c.Add(42)
	c.Inc()
	for _, s := range []float64{0.0005, 0.001, 0.002, 0.05, 3} {
		h.ObserveSeconds(s)
	}

	var sb strings.Builder
	if err := r.Write(&sb); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	want := `# HELP la_ingest_requests_total Requests by status code and format.
# TYPE la_ingest_requests_total counter
la_ingest_requests_total{code="200",format="native"} 2
la_ingest_requests_total{code="401",format="posthog"} 1
la_ingest_requests_total{code="0",format="other"} 1
# HELP la_ingest_bytes_in_total Body bytes accepted.
# TYPE la_ingest_bytes_in_total counter
la_ingest_bytes_in_total 43
# HELP la_ingest_fsync_seconds fsync duration.
# TYPE la_ingest_fsync_seconds histogram
la_ingest_fsync_seconds_bucket{le="0.001"} 2
la_ingest_fsync_seconds_bucket{le="0.01"} 3
la_ingest_fsync_seconds_bucket{le="0.1"} 4
la_ingest_fsync_seconds_bucket{le="+Inf"} 5
la_ingest_fsync_seconds_sum 3.0535
la_ingest_fsync_seconds_count 5
# HELP la_ingest_wal_local_bytes Local WAL bytes.
# TYPE la_ingest_wal_local_bytes gauge
la_ingest_wal_local_bytes 12345
# HELP la_ingest_tokens_age_seconds Age.
# TYPE la_ingest_tokens_age_seconds gauge
la_ingest_tokens_age_seconds 1.5
`
	if got != want {
		t.Fatalf("exposition mismatch:\n--- got\n%s--- want\n%s", got, want)
	}
	if req.Value(FormatNative, 200) != 2 || h.Count() != 5 || c.Value() != 43 {
		t.Fatal("accessors")
	}
}
