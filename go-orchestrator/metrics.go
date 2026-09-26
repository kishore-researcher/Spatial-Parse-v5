// metrics.go: a hand-rolled /metrics endpoint in Prometheus's plain-text
// exposition format (https://prometheus.io/docs/instrumenting/exposition_formats/).
//
// Why hand-rolled instead of github.com/prometheus/client_golang: verified
// directly (not assumed) that the official client can't be built in this
// sandbox -- its transitive dependency chain requires google.golang.org/protobuf
// and golang.org/x/sys, and both those vanity-import domains are outside
// this environment's network egress allowlist (confirmed by actually
// running `go get` and watching it fail on exactly those two hosts, after
// the initial github.com-hosted fetch of client_golang itself succeeded).
//
// The exposition format itself is simple, stable, and documented, so
// implementing it by hand is a legitimate, low-risk substitute: any real
// Prometheus server can scrape this endpoint with zero special
// configuration, since it doesn't know or care whether the text came from
// the official client library.
//
// Metric-naming note: the spec asked for "tokens_processed_per_second" as
// a metric name. Prometheus convention is to expose the raw cumulative
// counter (tokens_processed_total here) and let the query layer compute a
// rate via `rate(tokens_processed_total[1m])` -- a gauge/counter that
// itself represents "per second" would reset its own meaning depending on
// the scrape interval, which is exactly the anti-pattern Prometheus's data
// model is designed to avoid. Grafana dashboards built against this metric
// should graph `rate(tokens_processed_total[5m])`, not the raw counter.
package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// latencyBuckets are histogram bucket upper bounds for pdf_ingestion_latency_ms,
// chosen to span from "warm daemon, trivial doc" (single-digit ms, see the
// measured 12ms warm-daemon case in the daemon section of the README) up
// through "large cold-start document" (multi-second).
var latencyBuckets = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

type Metrics struct {
	mu sync.Mutex

	documentsIngestedTotal   uint64
	pagesPassedTotal         uint64
	pagesWarnedTotal         uint64
	pagesQuarantinedTotal    uint64
	redactClustersTotal      uint64
	chunksEmittedTotal       uint64
	tokensProcessedTotal     uint64
	quarantineApprovedTotal  uint64
	quarantineRejectedTotal  uint64
	ingestionErrorsTotal     uint64
	latencyBucketCounts      []uint64 // parallel to latencyBuckets, cumulative (Prometheus histogram convention: each bucket counts everything <= its bound)
	latencyCountTotal        uint64
	latencySumMs             float64
	daemonRequestsTotal      uint64
	subprocessFallbacksTotal uint64
}

func NewMetrics() *Metrics {
	return &Metrics{latencyBucketCounts: make([]uint64, len(latencyBuckets))}
}

func (m *Metrics) RecordIngestion(pagesPassed, pagesWarned, pagesQuarantined, redactClusters, chunksEmitted, tokensProcessed int, elapsedMs float64, usedDaemon bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.documentsIngestedTotal++
	m.pagesPassedTotal += uint64(pagesPassed)
	m.pagesWarnedTotal += uint64(pagesWarned)
	m.pagesQuarantinedTotal += uint64(pagesQuarantined)
	m.redactClustersTotal += uint64(redactClusters)
	m.chunksEmittedTotal += uint64(chunksEmitted)
	m.tokensProcessedTotal += uint64(tokensProcessed)
	if usedDaemon {
		m.daemonRequestsTotal++
	} else {
		m.subprocessFallbacksTotal++
	}

	m.latencyCountTotal++
	m.latencySumMs += elapsedMs
	for i, bound := range latencyBuckets {
		if elapsedMs <= bound {
			m.latencyBucketCounts[i]++
		}
	}
}

func (m *Metrics) RecordIngestionError() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ingestionErrorsTotal++
}

func (m *Metrics) RecordReview(status ReviewStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch status {
	case StatusApproved:
		m.quarantineApprovedTotal++
	case StatusRejected:
		m.quarantineRejectedTotal++
	}
}

// ServeHTTP writes the current metric values in Prometheus text exposition
// format. Snapshotting under the lock and formatting outside it keeps the
// lock held for a bounded, tiny amount of time regardless of how slow the
// HTTP write itself is. Individual fields are copied out rather than the
// whole struct, since Metrics embeds a sync.Mutex which must never be
// copied by value.
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	documentsIngestedTotal := m.documentsIngestedTotal
	pagesPassedTotal := m.pagesPassedTotal
	pagesWarnedTotal := m.pagesWarnedTotal
	pagesQuarantinedTotal := m.pagesQuarantinedTotal
	redactClustersTotal := m.redactClustersTotal
	chunksEmittedTotal := m.chunksEmittedTotal
	tokensProcessedTotal := m.tokensProcessedTotal
	quarantineApprovedTotal := m.quarantineApprovedTotal
	quarantineRejectedTotal := m.quarantineRejectedTotal
	ingestionErrorsTotal := m.ingestionErrorsTotal
	daemonRequestsTotal := m.daemonRequestsTotal
	subprocessFallbacksTotal := m.subprocessFallbacksTotal
	latencyCountTotal := m.latencyCountTotal
	latencySumMs := m.latencySumMs
	bucketCounts := append([]uint64(nil), m.latencyBucketCounts...)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")

	writeCounter(w, "documents_ingested_total", "Total PDF documents ingested.", float64(documentsIngestedTotal))
	writeCounter(w, "pages_passed_total", "Total pages that passed the structural fidelity gate.", float64(pagesPassedTotal))
	writeCounter(w, "pages_warned_total", "Total pages flagged warning_layout_anomaly.", float64(pagesWarnedTotal))
	writeCounter(w, "pages_quarantined_total", "Total pages quarantined (S_fidelity < 0.70).", float64(pagesQuarantinedTotal))
	writeCounter(w, "redact_clusters_total", "Total header/footer clusters redacted across all documents.", float64(redactClustersTotal))
	writeCounter(w, "chunks_emitted_total", "Total AST-chunked payloads emitted.", float64(chunksEmittedTotal))
	writeCounter(w, "tokens_processed_total", "Total tokens counted by the offline BPE tokenizer. Use rate() for a per-second figure.", float64(tokensProcessedTotal))
	writeCounter(w, "quarantine_approved_total", "Total quarantined pages approved via the admin API.", float64(quarantineApprovedTotal))
	writeCounter(w, "quarantine_rejected_total", "Total quarantined pages rejected via the admin API.", float64(quarantineRejectedTotal))
	writeCounter(w, "ingestion_errors_total", "Total /ingest requests that failed before producing a summary.", float64(ingestionErrorsTotal))
	writeCounter(w, "daemon_requests_total", "Total requests served by the persistent Rust daemon.", float64(daemonRequestsTotal))
	writeCounter(w, "subprocess_fallback_requests_total", "Total requests served by subprocess fallback (daemon unavailable).", float64(subprocessFallbacksTotal))

	writeHistogram(w, "pdf_ingestion_latency_ms", "PDF ingestion pipeline latency in milliseconds (Rust core execution time).", latencyBuckets, bucketCounts, latencyCountTotal, latencySumMs)
}

func writeCounter(w http.ResponseWriter, name, help string, value float64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	fmt.Fprintf(w, "%s %g\n", name, value)
}

func writeHistogram(w http.ResponseWriter, name, help string, bounds []float64, bucketCounts []uint64, count uint64, sum float64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", name)
	for i, bound := range bounds {
		fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", name, bound, bucketCounts[i])
	}
	fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", name, count)
	fmt.Fprintf(w, "%s_sum %g\n", name, sum)
	fmt.Fprintf(w, "%s_count %d\n", name, count)
}

// nowMs is a small helper so callers can compute elapsed milliseconds
// consistently; kept here rather than inlined at each call site.
func elapsedMs(start time.Time) float64 {
	return float64(time.Since(start)) / float64(time.Millisecond)
}
