package coordinator

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ogefest/hoarfrost/internal/recorder"
)

// Metrics exposes Prometheus metrics of the coordinator.
type Metrics struct {
	reg       *prometheus.Registry
	captures  *prometheus.CounterVec
	failures  *prometheus.CounterVec
	fallbacks *prometheus.CounterVec
	records   *prometheus.CounterVec
	bytes     prometheus.Counter
}

func newMetrics(c *Coordinator) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		captures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoarfrost_captures_total",
			Help: "Captures committed to the archive."}, []string{"source"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoarfrost_capture_failures_total",
			Help: "Capture attempts that reported an error."}, []string{"source"}),
		fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoarfrost_fallbacks_total",
			Help: "Captures retried with the fallback engine."}, []string{"source"}),
		records: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoarfrost_records_total",
			Help: "WARC records written, by type."}, []string{"type"}),
		bytes: prometheus.NewCounter(prometheus.CounterOpts{Name: "hoarfrost_capture_bytes_total",
			Help: "Bytes of capture files written to incoming/."}),
	}
	m.reg.MustRegister(m.captures, m.failures, m.fallbacks, m.records, m.bytes,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), &stateCollector{c})
	return m
}

func (m *Metrics) capture(source string, cf *recorder.CaptureFile) {
	m.captures.WithLabelValues(source).Inc()
	m.bytes.Add(float64(cf.Size))
	for _, r := range cf.Records {
		m.records.WithLabelValues(r.Type).Inc()
	}
}

func (m *Metrics) failure(source string)  { m.failures.WithLabelValues(source).Inc() }
func (m *Metrics) fallback(source string) { m.fallbacks.WithLabelValues(source).Inc() }

// Handler serves the metrics in Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}

var (
	descJobs     = prometheus.NewDesc("hoarfrost_queue_jobs", "Jobs by state.", []string{"state"}, nil)
	descEpochs   = prometheus.NewDesc("hoarfrost_archive_bytes", "Stored bytes per storage and epoch.", []string{"storage", "epoch", "state"}, nil)
	descPages    = prometheus.NewDesc("hoarfrost_pages", "Indexed pages.", nil, nil)
	descRevisits = prometheus.NewDesc("hoarfrost_revisit_records", "Deduplicated (revisit) records.", nil, nil)
	descSaved    = prometheus.NewDesc("hoarfrost_dedup_saved_bytes", "Bytes saved by deduplication (compressed size of originals).", nil, nil)
	descWorkers  = prometheus.NewDesc("hoarfrost_workers", "Workers seen in the last hour.", nil, nil)
	descBlocks   = prometheus.NewDesc("hoarfrost_blocklist_rules", "Rules in the global block lists.", nil, nil)
)

type stateCollector struct{ c *Coordinator }

func (s *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descJobs, descEpochs, descPages, descRevisits, descSaved, descWorkers, descBlocks} {
		ch <- d
	}
}

func (s *stateCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if counts, err := s.c.queue.Counts(ctx); err == nil {
		for state, n := range counts {
			ch <- prometheus.MustNewConstMetric(descJobs, prometheus.GaugeValue, float64(n), state)
		}
	}
	if st, err := s.c.idx.Stats(ctx, 1); err == nil {
		for _, e := range st.EpochSizes {
			ch <- prometheus.MustNewConstMetric(descEpochs, prometheus.GaugeValue, float64(e.Bytes), e.Storage, string(e.Epoch), e.State)
		}
		ch <- prometheus.MustNewConstMetric(descPages, prometheus.GaugeValue, float64(st.Pages))
		ch <- prometheus.MustNewConstMetric(descRevisits, prometheus.GaugeValue, float64(st.Revisits))
		ch <- prometheus.MustNewConstMetric(descSaved, prometheus.GaugeValue, float64(st.DedupSaved))
	}
	ch <- prometheus.MustNewConstMetric(descWorkers, prometheus.GaugeValue, float64(len(s.c.Workers())))
	ch <- prometheus.MustNewConstMetric(descBlocks, prometheus.GaugeValue, float64(s.c.blocks.Load().Set().Len()))
}
