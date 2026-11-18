// Package metrics is the control plane's small Prometheus registry (text
// exposition format 0.0.4): counters, labelled counters, callback gauges and
// fixed-bucket histograms, all under the la_control_ prefix.
package metrics

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Prefix is prepended to every metric name.
const Prefix = "la_control_"

// Counter is a monotonically increasing integer.
type Counter struct{ v atomic.Uint64 }

func (c *Counter) Inc()          { c.v.Add(1) }
func (c *Counter) Add(n uint64)  { c.v.Add(n) }
func (c *Counter) Value() uint64 { return c.v.Load() }

// CounterVec is a counter with one label.
type CounterVec struct {
	label string
	mu    sync.Mutex
	vals  map[string]*Counter
}

// With returns the counter of one label value.
func (v *CounterVec) With(value string) *Counter {
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.vals[value]
	if c == nil {
		c = &Counter{}
		v.vals[value] = c
	}
	return c
}

// Histogram counts observations (seconds) in fixed upper-bounded buckets.
type Histogram struct {
	bounds []float64
	counts []atomic.Uint64
	count  atomic.Uint64
	sumUs  atomic.Uint64
}

// Observe records one observation in seconds.
func (h *Histogram) Observe(seconds float64) {
	for i, b := range h.bounds {
		if seconds <= b {
			h.counts[i].Add(1)
			break
		}
	}
	h.count.Add(1)
	if seconds > 0 {
		h.sumUs.Add(uint64(seconds * 1e6))
	}
}

type entry struct {
	name, help, kind string
	write            func(w *bufio.Writer, name string)
}

// Registry holds metrics and renders them.
type Registry struct {
	mu      sync.Mutex
	entries []entry
}

// New returns an empty registry.
func New() *Registry { return &Registry{} }

func (r *Registry) add(name, help, kind string, write func(*bufio.Writer, string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, entry{name: Prefix + name, help: help, kind: kind, write: write})
}

// Counter registers a counter.
func (r *Registry) Counter(name, help string) *Counter {
	c := &Counter{}
	r.add(name, help, "counter", func(w *bufio.Writer, n string) {
		fmt.Fprintf(w, "%s %d\n", n, c.Value())
	})
	return c
}

// CounterVec registers a counter with one label.
func (r *Registry) CounterVec(name, help, label string) *CounterVec {
	v := &CounterVec{label: label, vals: map[string]*Counter{}}
	r.add(name, help, "counter", func(w *bufio.Writer, n string) {
		v.mu.Lock()
		keys := make([]string, 0, len(v.vals))
		for k := range v.vals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		vals := make([]uint64, len(keys))
		for i, k := range keys {
			vals[i] = v.vals[k].Value()
		}
		v.mu.Unlock()
		for i, k := range keys {
			fmt.Fprintf(w, "%s{%s=%q} %d\n", n, v.label, escapeLabel(k), vals[i])
		}
	})
	return v
}

// Gauge registers a gauge whose value is read at scrape time.
func (r *Registry) Gauge(name, help string, read func() float64) {
	r.add(name, help, "gauge", func(w *bufio.Writer, n string) {
		fmt.Fprintf(w, "%s %s\n", n, strconv.FormatFloat(read(), 'g', -1, 64))
	})
}

// CounterFunc registers a counter whose value is read at scrape time (for
// counts that are kept elsewhere).
func (r *Registry) CounterFunc(name, help string, read func() uint64) {
	r.add(name, help, "counter", func(w *bufio.Writer, n string) {
		fmt.Fprintf(w, "%s %d\n", n, read())
	})
}

// Histogram registers a histogram with the given upper bounds (seconds).
func (r *Registry) Histogram(name, help string, bounds ...float64) *Histogram {
	h := &Histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds))}
	r.add(name, help, "histogram", func(w *bufio.Writer, n string) {
		var cum uint64
		for i, b := range h.bounds {
			cum += h.counts[i].Load()
			fmt.Fprintf(w, "%s_bucket{le=%q} %d\n", n, strconv.FormatFloat(b, 'g', -1, 64), cum)
		}
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", n, h.count.Load())
		fmt.Fprintf(w, "%s_sum %s\n", n, strconv.FormatFloat(float64(h.sumUs.Load())/1e6, 'g', -1, 64))
		fmt.Fprintf(w, "%s_count %d\n", n, h.count.Load())
	})
	return h
}

func escapeLabel(s string) string {
	// %q already escapes quotes and backslashes; newlines must not appear raw.
	return strings.ReplaceAll(s, "\n", " ")
}

// Write renders every metric.
func (r *Registry) Write(out io.Writer) error {
	r.mu.Lock()
	entries := append([]entry(nil), r.entries...)
	r.mu.Unlock()
	w := bufio.NewWriter(out)
	for _, e := range entries {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", e.name, e.help, e.name, e.kind)
		e.write(w, e.name)
	}
	return w.Flush()
}
