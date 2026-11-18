// Package metrics is a minimal, allocation-free-on-update metrics registry
// with Prometheus text exposition (format 0.0.4). It implements only what
// ingestion needs: counters, callback gauges, fixed-bucket histograms and
// one specialised labelled counter.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync/atomic"
)

// Counter is a monotonically increasing integer.
type Counter struct{ v atomic.Uint64 }

func (c *Counter) Inc()          { c.v.Add(1) }
func (c *Counter) Add(n uint64)  { c.v.Add(n) }
func (c *Counter) Value() uint64 { return c.v.Load() }

// Histogram counts observations in fixed upper-bounded buckets.
type Histogram struct {
	bounds []float64 // upper bounds in seconds, ascending
	counts []atomic.Uint64
	count  atomic.Uint64
	sumNs  atomic.Uint64
}

// NewHistogram returns a histogram with the given upper bounds (seconds).
func NewHistogram(bounds ...float64) *Histogram {
	return &Histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds))}
}

// ObserveSeconds records one observation.
func (h *Histogram) ObserveSeconds(s float64) {
	for i, b := range h.bounds {
		if s <= b {
			h.counts[i].Add(1)
			break
		}
	}
	h.count.Add(1)
	if s > 0 {
		h.sumNs.Add(uint64(s * 1e9))
	}
}

// Count returns the number of observations.
func (h *Histogram) Count() uint64 { return h.count.Load() }

// Formats of the requests_total{format=...} label.
const (
	FormatNative = iota
	FormatPostHog
	FormatReplay
	FormatFlags
	FormatOther
	numFormats
)

var formatNames = [numFormats]string{"native", "posthog", "replay", "flags", "other"}

const maxCode = 600

// RequestCounter is requests_total{code,format}: a fixed table indexed by
// format and HTTP status, so the hot path is a single atomic add.
type RequestCounter struct {
	v [numFormats][maxCode]atomic.Uint64
}

// Inc counts one response.
func (r *RequestCounter) Inc(format, code int) {
	if format < 0 || format >= numFormats {
		format = FormatOther
	}
	if code < 0 || code >= maxCode {
		code = 0
	}
	r.v[format][code].Add(1)
}

// Value returns the count for one (format, code) pair.
func (r *RequestCounter) Value(format, code int) uint64 { return r.v[format][code].Load() }

type entry struct {
	name, help, typ string
	write           func(w *bufio.Writer, name string)
}

// Registry is an ordered set of metrics sharing a name prefix.
type Registry struct {
	prefix  string
	entries []entry
}

// NewRegistry returns a registry whose metric names all start with prefix.
func NewRegistry(prefix string) *Registry { return &Registry{prefix: prefix} }

func (r *Registry) add(name, help, typ string, write func(*bufio.Writer, string)) {
	r.entries = append(r.entries, entry{r.prefix + name, help, typ, write})
}

// Counter registers and returns a counter.
func (r *Registry) Counter(name, help string) *Counter {
	c := &Counter{}
	r.add(name, help, "counter", func(w *bufio.Writer, n string) {
		fmt.Fprintf(w, "%s %d\n", n, c.Value())
	})
	return c
}

// Gauge registers a gauge whose value is read from fn at scrape time.
func (r *Registry) Gauge(name, help string, fn func() float64) {
	r.add(name, help, "gauge", func(w *bufio.Writer, n string) {
		fmt.Fprintf(w, "%s %s\n", n, formatFloat(fn()))
	})
}

// Histogram registers and returns a histogram.
func (r *Registry) Histogram(name, help string, bounds ...float64) *Histogram {
	h := NewHistogram(bounds...)
	r.add(name, help, "histogram", func(w *bufio.Writer, n string) {
		var cum uint64
		for i, b := range h.bounds {
			cum += h.counts[i].Load()
			fmt.Fprintf(w, "%s_bucket{le=\"%s\"} %d\n", n, formatFloat(b), cum)
		}
		count := h.count.Load()
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", n, max(count, cum))
		fmt.Fprintf(w, "%s_sum %s\n", n, formatFloat(float64(h.sumNs.Load())/1e9))
		fmt.Fprintf(w, "%s_count %d\n", n, max(count, cum))
	})
	return h
}

// Requests registers and returns the requests_total{code,format} counter.
func (r *Registry) Requests(name, help string) *RequestCounter {
	rc := &RequestCounter{}
	r.add(name, help, "counter", func(w *bufio.Writer, n string) {
		for f := 0; f < numFormats; f++ {
			for code := 0; code < maxCode; code++ {
				if v := rc.v[f][code].Load(); v != 0 {
					fmt.Fprintf(w, "%s{code=\"%d\",format=\"%s\"} %d\n", n, code, formatNames[f], v)
				}
			}
		}
	})
	return rc
}

// Write renders every metric in Prometheus text format.
func (r *Registry) Write(out io.Writer) error {
	w := bufio.NewWriter(out)
	for _, e := range r.entries {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", e.name, e.help, e.name, e.typ)
		e.write(w, e.name)
	}
	return w.Flush()
}

func formatFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "+Inf"
	case math.IsInf(f, -1):
		return "-Inf"
	case math.IsNaN(f):
		return "NaN"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}
