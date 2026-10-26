package coordinator

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/worker"
)

// Metrics exposes Prometheus metrics of the coordinator.
type Metrics struct {
	reg       *prometheus.Registry
	captures  *prometheus.CounterVec
	failures  *prometheus.CounterVec
	polls     *prometheus.CounterVec
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
			Help: "Capture attempts that reported an error, by reason (http_401, http_403, http_404, http_410, http_429, http_4xx, http_5xx, timeout, network, blocked, storage, blocklist, engine, other)."},
			[]string{"source", "reason"}),
		polls: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoarfrost_source_poll_errors_total",
			Help: "Polls of a source (feeds, sitemaps) that failed or partly failed."}, []string{"source"}),
		fallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoarfrost_fallbacks_total",
			Help: "Captures retried with the fallback engine."}, []string{"source"}),
		records: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "hoarfrost_records_total",
			Help: "WARC records written, by type."}, []string{"type"}),
		bytes: prometheus.NewCounter(prometheus.CounterOpts{Name: "hoarfrost_capture_bytes_total",
			Help: "Bytes of capture files written to incoming/."}),
	}
	m.reg.MustRegister(m.captures, m.failures, m.polls, m.fallbacks, m.records, m.bytes,
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

func (m *Metrics) failure(source, reason string) {
	if reason == "" {
		reason = worker.ReasonOther // workers of older versions do not send one
	}
	m.failures.WithLabelValues(source, reason).Inc()
}

func (m *Metrics) pollError(source string) { m.polls.WithLabelValues(source).Inc() }
func (m *Metrics) fallback(source string)  { m.fallbacks.WithLabelValues(source).Inc() }

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

	descBlockFailing = prometheus.NewDesc("hoarfrost_blocklist_failing", "1 when the last refresh of a block list failed (the previous copy stays in use).", []string{"list"}, nil)
	descSrcPoll      = prometheus.NewDesc("hoarfrost_source_last_poll_timestamp_seconds", "Time of the last poll of an enabled source.", []string{"source"}, nil)
	descSrcSuccess   = prometheus.NewDesc("hoarfrost_source_last_success_timestamp_seconds", "Time of the last poll without errors of an enabled source.", []string{"source"}, nil)
	descSrcFailing   = prometheus.NewDesc("hoarfrost_source_failing", "1 when the last poll of an enabled source reported an error.", []string{"source"}, nil)
	descSrcFound     = prometheus.NewDesc("hoarfrost_source_urls_found", "URLs found in the feeds and sitemaps of an enabled source by its last poll.", []string{"source"}, nil)
	descSrcInterval  = prometheus.NewDesc("hoarfrost_source_poll_interval_seconds", "Poll interval of an enabled source.", []string{"source"}, nil)
	descEpochStates  = prometheus.NewDesc("hoarfrost_epochs", "Weeks by state (open, sealing, seal_failed, sealed, verified, failed: the package failed verification).", []string{"storage", "state"}, nil)
	descOldestOpen   = prometheus.NewDesc("hoarfrost_oldest_unsealed_epoch_end_timestamp_seconds", "End of the oldest week of a storage that is not sealed yet.", []string{"storage"}, nil)
	descSealErrors   = prometheus.NewDesc("hoarfrost_seal_errors", "Weeks whose last sealing attempt failed or was interrupted (kept across restarts until the week is sealed).", []string{"storage"}, nil)
)

type stateCollector struct{ c *Coordinator }

func (s *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descJobs, descEpochs, descPages, descRevisits, descSaved, descWorkers, descBlocks,
		descBlockFailing, descSrcPoll, descSrcSuccess, descSrcFailing, descSrcFound, descSrcInterval, descEpochStates, descOldestOpen, descSealErrors} {
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
	for _, st := range s.c.BlocklistStatuses() {
		ch <- prometheus.MustNewConstMetric(descBlockFailing, prometheus.GaugeValue, boolValue(st.Error != ""), st.URL)
	}
	s.collectSources(ctx, ch)
	s.collectEpochs(ctx, ch)
}

// collectSources reports the poll state of enabled sources; disabled ones
// have no series, so alerts need no exceptions for them.
func (s *stateCollector) collectSources(ctx context.Context, ch chan<- prometheus.Metric) {
	statuses, err := s.c.opts.Sources.Statuses(ctx)
	if err != nil {
		return
	}
	src := s.c.Sources()
	for _, so := range src.Sources {
		if !so.IsEnabled() {
			continue
		}
		ch <- prometheus.MustNewConstMetric(descSrcInterval, prometheus.GaugeValue, src.Resolve(so.Name).Poll.Seconds(), so.Name)
		st, ok := statuses[so.Name]
		if !ok {
			continue // not polled yet
		}
		ch <- prometheus.MustNewConstMetric(descSrcPoll, prometheus.GaugeValue, unix(st.LastPoll), so.Name)
		if !st.LastSuccess.IsZero() {
			ch <- prometheus.MustNewConstMetric(descSrcSuccess, prometheus.GaugeValue, unix(st.LastSuccess), so.Name)
		}
		ch <- prometheus.MustNewConstMetric(descSrcFailing, prometheus.GaugeValue, boolValue(st.Error != ""), so.Name)
		ch <- prometheus.MustNewConstMetric(descSrcFound, prometheus.GaugeValue, float64(st.Found), so.Name)
	}
}

// collectEpochs reports weeks by state, the oldest unsealed week and failed
// sealing attempts per storage.
func (s *stateCollector) collectEpochs(ctx context.Context, ch chan<- prometheus.Metric) {
	eps, err := s.c.idx.Epochs(ctx)
	if err != nil {
		return
	}
	type key struct{ storage, state string }
	counts := map[key]int{}
	oldest := map[string]time.Time{}
	for _, e := range eps {
		state := e.State
		if state == index.StateFailed && e.WACZKey == "" {
			state = "seal_failed" // sealing failed; "failed" is a package that failed verification
		}
		counts[key{e.Storage, state}]++
		if state == index.StateOpen || state == index.StateSealing || state == "seal_failed" {
			if end := e.Epoch.End(); oldest[e.Storage].IsZero() || end.Before(oldest[e.Storage]) {
				oldest[e.Storage] = end
			}
		}
	}
	for k, n := range counts {
		ch <- prometheus.MustNewConstMetric(descEpochStates, prometheus.GaugeValue, float64(n), k.storage, k.state)
	}
	for st, end := range oldest {
		ch <- prometheus.MustNewConstMetric(descOldestOpen, prometheus.GaugeValue, unix(end), st)
	}
	sealErrs := map[string]int{}
	for _, name := range s.c.Storages() {
		sealErrs[index.StorageOr(name)] = 0
	}
	for k := range s.c.SealErrors(ctx) {
		storage, _, _ := strings.Cut(k, "/")
		sealErrs[storage]++
	}
	for st, n := range sealErrs {
		ch <- prometheus.MustNewConstMetric(descSealErrors, prometheus.GaugeValue, float64(n), st)
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func unix(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }
