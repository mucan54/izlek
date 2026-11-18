package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestExposition(t *testing.T) {
	r := New()
	c := r.Counter("things_total", "Things.")
	v := r.CounterVec("requests_total", "Requests.", "code")
	h := r.Histogram("request_seconds", "Latency.", 0.1, 1)
	r.Gauge("lag_seconds", "Lag.", func() float64 { return 1.5 })
	r.CounterFunc("published_total", "Published.", func() uint64 { return 7 })
	c.Add(3)
	v.With("200").Inc()
	v.With("200").Inc()
	v.With("404").Inc()
	h.Observe(0.05)
	h.Observe(0.5)
	h.Observe(5)

	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP la_control_things_total Things.
# TYPE la_control_things_total counter
la_control_things_total 3
# HELP la_control_requests_total Requests.
# TYPE la_control_requests_total counter
la_control_requests_total{code="200"} 2
la_control_requests_total{code="404"} 1
# HELP la_control_request_seconds Latency.
# TYPE la_control_request_seconds histogram
la_control_request_seconds_bucket{le="0.1"} 1
la_control_request_seconds_bucket{le="1"} 2
la_control_request_seconds_bucket{le="+Inf"} 3
la_control_request_seconds_sum 5.55
la_control_request_seconds_count 3
# HELP la_control_lag_seconds Lag.
# TYPE la_control_lag_seconds gauge
la_control_lag_seconds 1.5
# HELP la_control_published_total Published.
# TYPE la_control_published_total counter
la_control_published_total 7
`
	if b.String() != want {
		t.Fatalf("exposition:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New()
	v := r.CounterVec("requests_total", "Requests.", "code")
	h := r.Histogram("seconds", "Latency.", 1)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				v.With("200").Inc()
				h.Observe(0.5)
				var b strings.Builder
				_ = r.Write(&b)
			}
		}()
	}
	wg.Wait()
	if got := v.With("200").Value(); got != 4000 {
		t.Fatalf("counter = %d", got)
	}
}
