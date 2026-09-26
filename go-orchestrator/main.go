// Command rag-ingestion-orchestrator is the network/orchestration layer of
// the pipeline: multipart upload handling, Rust core dispatch (subprocess
// or persistent daemon), and -- as of this revision -- the enterprise
// productization layer built on top: a multi-tenant quarantine review API,
// Prometheus-compatible metrics, structured tracing, and vector database
// export connectors. See store.go, admin_api.go, metrics.go, tracing.go,
// and export*.go for each piece; this file wires them together.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const maxUploadBytes = 2 << 30 // 2GB worst-case guard; tune per deployment

func rustCorePath() string {
	if p := os.Getenv("RUST_CORE_PATH"); p != "" {
		return p
	}
	return "../rust-core/target/release/rag_ingestion_core"
}

// rustDaemonSocket returns the configured Unix Domain Socket path for the
// persistent Rust worker daemon (Track A Priority 4), or "" if daemon mode
// isn't configured -- in which case every request falls back to the
// subprocess-spawn path (streamRustCoreSubprocess) unchanged.
func rustDaemonSocket() string {
	return os.Getenv("RUST_DAEMON_SOCKET")
}

// Server holds the orchestrator's shared, long-lived dependencies. Methods
// on *Server (rather than free functions closing over package-level
// globals) keep the whole thing constructible and testable without a
// running process -- see main_test.go.
type Server struct {
	store   *Store
	docs    *DocumentStore
	metrics *Metrics
	auth    *TenantAuth
	admin   *AdminAPI
	export  *ExportAPI
	docsAPI *DocumentsAPI
}

func NewServer() *Server {
	storePath := os.Getenv("QUARANTINE_STORE_PATH")
	if storePath == "" {
		storePath = "quarantine_store.json"
	}
	store, err := NewStore(storePath)
	if err != nil {
		log.Fatalf("failed to open quarantine store at %s: %v", storePath, err)
	}

	dataDir := os.Getenv("DATA_DIR")
	docs, err := NewDocumentStore(dataDir)
	if err != nil {
		log.Fatalf("failed to open document store at %q: %v", dataDir, err)
	}

	return &Server{
		store:   store,
		docs:    docs,
		metrics: NewMetrics(),
		auth:    loadTenantAuth(),
		admin:   NewAdminAPI(store),
		export:  NewExportAPI(store),
		docsAPI: NewDocumentsAPI(docs),
	}
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/metrics", s.metrics.ServeHTTP) // unauthenticated: an internal-network scrape endpoint for Prometheus, not a tenant-facing API
	mux.HandleFunc("/", serveDashboard)             // GET / and GET /dashboard both serve the embedded UI (see dashboard.go)
	mux.HandleFunc("/dashboard", serveDashboard)
	mux.HandleFunc("/ingest", s.auth.requireTenant(s.handleIngest))
	mux.HandleFunc("/api/v1/quarantine", s.auth.requireTenant(s.admin.Route))
	mux.HandleFunc("/api/v1/quarantine/", s.auth.requireTenant(s.admin.Route))
	mux.HandleFunc("/api/v1/export", s.auth.requireTenant(s.export.Handle))
	mux.HandleFunc("/api/v1/documents", s.auth.requireTenant(s.docsAPI.Route))
	mux.HandleFunc("/api/v1/documents/", s.auth.requireTenant(s.docsAPI.Route))
	return mux
}

func main() {
	server := NewServer()

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           server.routes(),
		ReadHeaderTimeout: 15 * time.Second,
		// No response-body write timeout: large-document ingestion is
		// intentionally long-running and streamed.
	}
	log.Printf("rag-ingestion-orchestrator listening on %s (rust core: %s, daemon socket: %q [empty = subprocess mode])", addr, rustCorePath(), rustDaemonSocket())
	log.Fatal(srv.ListenAndServe())
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat(rustCorePath()); err != nil {
		http.Error(w, fmt.Sprintf(`{"status":"degraded","reason":"rust core not found at %s"}`, rustCorePath()), http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if socketPath := rustDaemonSocket(); socketPath != "" {
		conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
		if err != nil {
			fmt.Fprintf(w, `{"status":"ok","daemon":"unreachable (%s), falling back to subprocess per-request"}`, err.Error())
			return
		}
		conn.Close()
		fmt.Fprint(w, `{"status":"ok","daemon":"connected"}`)
		return
	}
	fmt.Fprint(w, `{"status":"ok","daemon":"not configured, using subprocess per-request"}`)
}

// handleIngest streams the uploaded PDF straight to disk via a true
// multipart.Reader (never buffering the whole request body in Go's heap
// the way http.Request.ParseMultipartForm's default memory threshold can),
// then dispatches to the Rust core (daemon preferred, subprocess fallback)
// and streams its NDJSON output back to the client as it's produced --
// while also inspecting each record to update metrics and persist
// quarantined pages into the admin review store.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	span := StartSpan("ingest_request", nil)
	tenantID := tenantFromContext(r)
	span.SetAttribute("tenant_id", tenantID)
	defer span.End()

	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)

	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		http.Error(w, "expected multipart/form-data with a 'file' field", http.StatusBadRequest)
		return
	}
	boundary, ok := params["boundary"]
	if !ok {
		http.Error(w, "missing multipart boundary", http.StatusBadRequest)
		return
	}

	uploadSpan := StartSpan("receive_upload", span)
	mr := multipart.NewReader(r.Body, boundary)
	documentID := "document"
	tmpFile, err := os.CreateTemp("", "ingest-*.pdf")
	if err != nil {
		uploadSpan.SetError(err)
		uploadSpan.End()
		http.Error(w, "failed to allocate temp storage", http.StatusInternalServerError)
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	var fileReceived bool
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			tmpFile.Close()
			uploadSpan.SetError(err)
			uploadSpan.End()
			http.Error(w, "malformed multipart body", http.StatusBadRequest)
			return
		}
		switch part.FormName() {
		case "file":
			// Streams directly from the request body to disk in bounded
			// chunks (io.Copy's default 32KB buffer) rather than reading
			// the whole part into a []byte first.
			if _, err := io.Copy(tmpFile, part); err != nil {
				tmpFile.Close()
				uploadSpan.SetError(err)
				uploadSpan.End()
				http.Error(w, "failed while streaming upload to disk", http.StatusInternalServerError)
				return
			}
			fileReceived = true
		case "document_id":
			buf := make([]byte, 256)
			n, _ := part.Read(buf)
			if n > 0 {
				documentID = strings.TrimSpace(string(buf[:n]))
			}
		}
		part.Close()
	}
	tmpFile.Close()
	uploadSpan.SetAttribute("document_id", documentID)
	uploadSpan.End()

	if !fileReceived {
		s.metrics.RecordIngestionError()
		http.Error(w, "no 'file' part found in multipart body", http.StatusBadRequest)
		return
	}
	span.SetAttribute("document_id", documentID)

	tracker := &ingestTracker{store: s.store, docs: s.docs, metrics: s.metrics, tenantID: tenantID, documentID: documentID, sourcePDF: tmpPath, start: time.Now()}

	// Prefer the persistent daemon (Track A Priority 4) when configured: it
	// eliminates the ~150-300ms process-spawn + tokenizer-reparse cost that
	// dominates a cold subprocess invocation, typically dropping per-request
	// latency to single-digit milliseconds once the daemon is warm (measured
	// during development: ~200ms first request, ~4-30ms every request after,
	// including under concurrent load). Falls back to spawning a subprocess
	// automatically -- and logs why -- if the daemon isn't reachable, so a
	// daemon crash or intentionally daemon-less deployment degrades to the
	// always-correct baseline rather than failing the request.
	if socketPath := rustDaemonSocket(); socketPath != "" {
		dispatchSpan := StartSpan("dispatch_daemon", span)
		if err := streamRustCoreDaemon(w, tmpPath, documentID, socketPath, tracker); err == nil {
			tracker.finish(true)
			dispatchSpan.End()
			return
		} else {
			dispatchSpan.SetError(err)
			dispatchSpan.End()
			log.Printf("daemon unavailable (%v), falling back to subprocess for this request", err)
		}
	}
	subprocessSpan := StartSpan("dispatch_subprocess", span)
	streamRustCoreSubprocess(w, tmpPath, documentID, tracker)
	tracker.finish(false)
	subprocessSpan.End()
}

// ingestTracker inspects each NDJSON record as it's relayed to the HTTP
// client -- without altering what's relayed -- to (a) persist quarantined
// pages into the admin review store (Feature 1) and (b) accumulate the
// counts finish() reports to Metrics (Feature 2). This is what lets both
// features work identically regardless of which dispatch path (daemon or
// subprocess) served the request.
type ingestTracker struct {
	store      *Store
	docs       *DocumentStore
	metrics    *Metrics
	tenantID   string
	documentID string
	sourcePDF  string
	start      time.Time

	pagesPassed      int
	pagesWarned      int
	pagesQuarantined int
	redactClusters   int
	chunksEmitted    int
	tokensProcessed  int
	totalPages       int

	rawChunks   []json.RawMessage // every "chunk" record verbatim, for chunks.json
	rawFidelity []json.RawMessage // every "page_fidelity" record verbatim, for audit_report.json
	stitchParts []stitchPart      // (parent_hierarchy, text) per chunk, in emission order, for full_extracted.md
}

// stitchPart is one chunk's contribution to full_extracted.md.
type stitchPart struct {
	ParentHierarchy []string
	Text            string
}

type pageFidelityLine struct {
	Type   string `json:"type"`
	Report struct {
		PageNum          int     `json:"page_num"`
		CRetention       float64 `json:"c_retention"`
		TMatrix          float64 `json:"t_matrix"`
		SFidelity        float64 `json:"s_fidelity"`
		Quality          string  `json:"quality"`
		TableDetectedPDF bool    `json:"table_detected_in_pdf"`
		TableDetectedMD  bool    `json:"table_detected_in_md"`
	} `json:"report"`
	Markdown string `json:"markdown"`
}

type chunkLine struct {
	Type    string `json:"type"`
	Payload struct {
		TokenCount      int      `json:"token_count"`
		Text            string   `json:"text"`
		ParentHierarchy []string `json:"parent_hierarchy"`
	} `json:"payload"`
}

type summaryLine struct {
	Type             string `json:"type"`
	TotalPages       int    `json:"total_pages"`
	PagesPassed      int    `json:"pages_passed"`
	PagesWarned      int    `json:"pages_warned"`
	PagesQuarantined int    `json:"pages_quarantined"`
	RedactClusters   int    `json:"redact_clusters"`
	ChunksEmitted    int    `json:"chunks_emitted"`
}

// inspectLine is called once per NDJSON line as it streams through. Cheap
// even for large documents: one small JSON unmarshal per line, and only
// the record types we care about do any further work.
func (t *ingestTracker) inspectLine(line string) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return // not JSON (shouldn't happen for well-formed lines) -- ignore rather than fail the whole relay
	}

	switch probe.Type {
	case "page_fidelity":
		t.rawFidelity = append(t.rawFidelity, append(json.RawMessage(nil), line...))
		var pf pageFidelityLine
		if err := json.Unmarshal([]byte(line), &pf); err != nil {
			return
		}
		if pf.Report.Quality == "quarantined" {
			_, err := t.store.Add(PageRecord{
				TenantID:         t.tenantID,
				DocumentID:       t.documentID,
				PageNum:          pf.Report.PageNum,
				SFidelity:        pf.Report.SFidelity,
				CRetention:       pf.Report.CRetention,
				TMatrix:          pf.Report.TMatrix,
				TableDetectedPDF: pf.Report.TableDetectedPDF,
				TableDetectedMD:  pf.Report.TableDetectedMD,
				Markdown:         pf.Markdown,
			})
			if err != nil {
				log.Printf("failed to persist quarantine record for %s page %d: %v", t.documentID, pf.Report.PageNum, err)
			}
		}
	case "chunk":
		t.rawChunks = append(t.rawChunks, append(json.RawMessage(nil), line...))
		var c chunkLine
		if err := json.Unmarshal([]byte(line), &c); err == nil {
			t.tokensProcessed += c.Payload.TokenCount
			t.stitchParts = append(t.stitchParts, stitchPart{ParentHierarchy: c.Payload.ParentHierarchy, Text: c.Payload.Text})
		}
	case "summary":
		var s summaryLine
		if err := json.Unmarshal([]byte(line), &s); err == nil {
			t.totalPages = s.TotalPages
			t.pagesPassed = s.PagesPassed
			t.pagesWarned = s.PagesWarned
			t.pagesQuarantined = s.PagesQuarantined
			t.redactClusters = s.RedactClusters
			t.chunksEmitted = s.ChunksEmitted
		}
	}
}

func (t *ingestTracker) finish(usedDaemon bool) {
	elapsed := elapsedMs(t.start)
	t.metrics.RecordIngestion(t.pagesPassed, t.pagesWarned, t.pagesQuarantined, t.redactClusters, t.chunksEmitted, t.tokensProcessed, elapsed, usedDaemon)

	if t.docs == nil {
		return
	}
	result := IngestResult{
		Spans: TraceSpans{
			DocumentID: t.documentID, TenantID: t.tenantID, IngestedAt: time.Now().UTC(),
			ElapsedMs: elapsed, UsedDaemon: usedDaemon, TotalPages: t.totalPages,
			PagesPassed: t.pagesPassed, PagesWarned: t.pagesWarned, PagesQuarantined: t.pagesQuarantined,
			RedactClusters: t.redactClusters, ChunksEmitted: t.chunksEmitted, TokensProcessed: t.tokensProcessed,
		},
		ChunksJSON:    marshalRawArray(t.rawChunks),
		AuditJSON:     marshalRawArray(t.rawFidelity),
		StitchedMD:    buildStitchedMarkdown(t.stitchParts),
		SourcePDFPath: t.sourcePDF,
	}
	if err := t.docs.SaveIngestResult(t.tenantID, t.documentID, result); err != nil {
		log.Printf("failed to persist document store output for %s/%s: %v", t.tenantID, t.documentID, err)
	}
}

func marshalRawArray(items []json.RawMessage) []byte {
	if items == nil {
		items = []json.RawMessage{}
	}
	data, err := json.Marshal(items)
	if err != nil {
		return []byte("[]")
	}
	return data
}

// buildStitchedMarkdown reconstructs one readable document from chunk
// texts in emission order, re-inserting Markdown headers for each level of
// parent_hierarchy whenever it changes from the previous chunk -- so the
// stitched file reads like the original document's structure rather than
// a flat concatenation, while never repeating an unchanged heading.
func buildStitchedMarkdown(parts []stitchPart) string {
	var b strings.Builder
	var prevHierarchy []string
	for _, p := range parts {
		for depth, heading := range p.ParentHierarchy {
			if depth < len(prevHierarchy) && prevHierarchy[depth] == heading {
				continue // unchanged at this depth -- don't repeat it
			}
			b.WriteString(strings.Repeat("#", depth+1))
			b.WriteString(" ")
			b.WriteString(heading)
			b.WriteString("\n\n")
		}
		prevHierarchy = p.ParentHierarchy // update once per chunk, after all its levels are written -- not per depth
		b.WriteString(p.Text)
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}

// streamRustCoreDaemon sends the already-on-disk PDF to the persistent Rust
// daemon over a Unix Domain Socket and relays its NDJSON response to the
// HTTP client, line by line through tracker.inspectLine. Dials a fresh
// connection per request rather than sharing one persistent connection
// across concurrent HTTP requests: the wire protocol is strictly
// one-request-then-one-response per connection (see rust-core/src/daemon.rs),
// so concurrent requests sharing a connection would corrupt each other's
// framing. A fresh Unix socket connect is a local, sub-millisecond
// operation -- nothing like the ~150-300ms process spawn this whole
// mechanism exists to avoid -- so this trade-off costs essentially nothing
// while avoiding a connection-pool's added complexity and failure modes.
//
// Trade-off vs. the subprocess path: this buffers the *entire* NDJSON
// response before relaying any of it (the daemon's framing protocol sends
// one length-prefixed blob per request, not a byte stream), so a caller
// loses the "see page 1's results before page 500 is parsed" incremental
// behavior streamRustCoreSubprocess provides. Given the daemon's own
// measured per-request latency is single-digit-to-low-tens of
// milliseconds once warm, that's a reasonable trade for eliminating
// process-spawn overhead -- but it is a real behavioral difference, not
// just an implementation detail.
func streamRustCoreDaemon(w http.ResponseWriter, filePath, documentID, socketPath string, tracker *ingestTracker) error {
	pdfBytes, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("reading upload for daemon dispatch: %w", err)
	}

	conn, err := net.DialTimeout("unix", socketPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("dialing daemon socket: %w", err)
	}
	defer conn.Close()

	if err := writeFrame(conn, []byte(documentID)); err != nil {
		return fmt.Errorf("sending document_id frame: %w", err)
	}
	if err := writeFrame(conn, pdfBytes); err != nil {
		return fmt.Errorf("sending pdf frame: %w", err)
	}

	response, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("reading daemon response: %w", err)
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	for _, line := range strings.Split(string(response), "\n") {
		if line == "" {
			continue
		}
		tracker.inspectLine(line)
		fmt.Fprintln(w, line)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

// writeFrame/readFrame implement the daemon's simple length-prefixed
// framing (4-byte big-endian length, then that many bytes) -- see the wire
// protocol documented in rust-core/src/daemon.rs.
func writeFrame(conn net.Conn, data []byte) error {
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(data)))
	if _, err := conn.Write(lenBuf); err != nil {
		return err
	}
	_, err := conn.Write(data)
	return err
}

func readFrame(conn net.Conn) ([]byte, error) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(conn, lenBuf); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(lenBuf)
	const maxFrameBytes = 1 << 30 // 1GB guard, mirrors the daemon's own MAX_FRAME_BYTES
	if length > maxFrameBytes {
		return nil, fmt.Errorf("frame length %d exceeds %d byte guard", length, maxFrameBytes)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// streamRustCoreSubprocess spawns the Rust processing core against the given file
// path and relays both its NDJSON stdout (page_fidelity / chunk / summary
// records) and its stderr log lines (pass1/pass2 stats, quarantine ALERTs)
// to the HTTP client as they're produced, flushing after every line, and
// through tracker.inspectLine for stdout records.
func streamRustCoreSubprocess(w http.ResponseWriter, filePath, documentID string, tracker *ingestTracker) {
	flusher, canFlush := w.(http.Flusher)

	cmd := exec.Command(rustCorePath(), filePath, documentID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		http.Error(w, "failed to open core stdout", http.StatusInternalServerError)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		http.Error(w, "failed to open core stderr", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, `{"type":"fatal_error","message":%q}`+"\n", err.Error())
		return
	}

	done := make(chan struct{})
	var writeMu sync.Mutex
	safeWrite := func(format string, args ...interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		fmt.Fprintf(w, format, args...)
		if canFlush {
			flusher.Flush()
		}
	}

	go func() {
		defer close(done)
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			safeWrite("{\"type\":\"log\",\"message\":%q}\n", scanner.Text())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 256*1024), 8*1024*1024) // pages with big tables can produce long lines
	for scanner.Scan() {
		line := scanner.Text()
		tracker.inspectLine(line)
		safeWrite("%s\n", line)
	}

	<-done
	if err := cmd.Wait(); err != nil {
		safeWrite("{\"type\":\"fatal_error\",\"message\":%q}\n", err.Error())
	}
}
