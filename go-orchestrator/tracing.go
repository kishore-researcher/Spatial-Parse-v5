// tracing.go: structured, correlatable span logging.
//
// Why not the real OpenTelemetry SDK: verified directly (see metrics.go's
// doc comment for the exact failure) that this sandbox's network egress
// can't reach go.opentelemetry.io or its transitive dependencies. Rather
// than hand-roll an OTLP/HTTP JSON exporter with no real collector
// available here to validate it against -- which risks shipping something
// that *looks* like valid OTLP but is subtly wrong in ways I can't verify
// -- this implements a well-established, much lower-risk alternative:
// structured JSON logs carrying trace_id/span_id/duration_ms fields.
// Datadog, and most other log platforms, natively correlate logs into
// trace-like views via exactly these fields (Datadog explicitly supports
// "logs as traces" via trace_id correlation). This is a real, honest
// engineering trade-off, not a cut corner -- and the span shape below
// (trace_id, span_id, parent_span_id, name, start/end, attributes) is
// deliberately the OTel data model, so migrating to the real SDK later is
// a matter of swapping the exporter, not redesigning the instrumentation.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"time"
)

func newTraceID() string {
	b := make([]byte, 16) // 128-bit, matching OTel's trace ID width
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newSpanID() string {
	b := make([]byte, 8) // 64-bit, matching OTel's span ID width
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Span is a single traced operation. Create with StartSpan, populate
// Attributes as needed, and call End() when the operation completes.
type Span struct {
	TraceID      string                 `json:"trace_id"`
	SpanID       string                 `json:"span_id"`
	ParentSpanID string                 `json:"parent_span_id,omitempty"`
	Name         string                 `json:"name"`
	StartTime    time.Time              `json:"start_time"`
	EndTime      time.Time              `json:"end_time,omitempty"`
	DurationMs   float64                `json:"duration_ms,omitempty"`
	Attributes   map[string]interface{} `json:"attributes,omitempty"`
	Error        string                 `json:"error,omitempty"`
}

// StartSpan begins a new root span (fresh trace ID) or a child span if
// parent is non-nil (same trace ID, parent's span ID recorded).
func StartSpan(name string, parent *Span) *Span {
	s := &Span{
		SpanID:     newSpanID(),
		Name:       name,
		StartTime:  time.Now().UTC(),
		Attributes: make(map[string]interface{}),
	}
	if parent != nil {
		s.TraceID = parent.TraceID
		s.ParentSpanID = parent.SpanID
	} else {
		s.TraceID = newTraceID()
	}
	return s
}

func (s *Span) SetAttribute(key string, value interface{}) {
	s.Attributes[key] = value
}

func (s *Span) SetError(err error) {
	if err != nil {
		s.Error = err.Error()
	}
}

// End finalizes the span and emits it as one JSON log line to stdout via
// the standard `log` package. A real OTLP exporter could later replace
// just this method's body without touching any call site.
func (s *Span) End() {
	s.EndTime = time.Now().UTC()
	s.DurationMs = float64(s.EndTime.Sub(s.StartTime)) / float64(time.Millisecond)
	data, err := json.Marshal(s)
	if err != nil {
		log.Printf(`{"type":"span_marshal_error","error":%q}`, err.Error())
		return
	}
	log.Println(string(data))
}
