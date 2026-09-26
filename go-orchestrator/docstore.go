// docstore.go: Track D's local storage persistence layer. Every completed
// /ingest call writes its results to a structured, human-readable
// directory tree:
//
//	data/tenants/{tenant_id}/documents/{document_id}/
//	  source.pdf           -- the original upload, byte-for-byte
//	  full_extracted.md    -- stitched markdown, reconstructed from chunk texts in order
//	  chunks.json          -- the complete emitted chunk array, verbatim from the Rust core
//	  audit_report.json    -- every page_fidelity record (score + quarantine markdown when present)
//	  trace_spans.json     -- duration/token/page-count summary for this ingestion
//
// This is deliberately a plain directory tree, not a database -- consistent
// with store.go's file-backed choice for the same "light" scope, and it
// gets a real, tangible benefit for free: the files are directly readable
// (cat full_extracted.md) without going through any API at all, which
// matters for the stated goal of replacing "copying JSON payloads manually
// into LLMs" with something you can just point a tool at.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type DocumentStore struct {
	baseDir string
}

func NewDocumentStore(baseDir string) (*DocumentStore, error) {
	if baseDir == "" {
		baseDir = "data"
	}
	if err := os.MkdirAll(filepath.Join(baseDir, "tenants"), 0o755); err != nil {
		return nil, err
	}
	return &DocumentStore{baseDir: baseDir}, nil
}

// unsafePathComponent matches anything that isn't a conservative safe
// character for a single path segment.
var unsafePathComponent = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizePathComponent turns an arbitrary, user-supplied string (a
// tenant ID from an API key mapping, or a document_id from a multipart
// form field) into a single safe path segment. This is a real security
// boundary, not a cosmetic cleanup: document_id in particular arrives
// directly from client-controlled multipart form data, so a value like
// "../../../etc" must never be allowed to escape the intended directory.
// Collapsing ".." and stripping path separators, then falling back to a
// fixed placeholder if nothing safe remains, closes that off entirely --
// verified by a dedicated test (TestSanitizePathComponentBlocksTraversal).
func sanitizePathComponent(s string) string {
	s = strings.ReplaceAll(s, "..", "_")
	s = unsafePathComponent.ReplaceAllString(s, "_")
	s = strings.Trim(s, "._")
	if s == "" {
		s = "_"
	}
	const maxLen = 200 // filesystem-friendly; document IDs aren't expected to be long
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

func (d *DocumentStore) docDir(tenantID, documentID string) string {
	return filepath.Join(d.baseDir, "tenants", sanitizePathComponent(tenantID), "documents", sanitizePathComponent(documentID))
}

// TraceSpans is the content of trace_spans.json -- an ingestion-level
// summary, not per-operation OTel spans (those are the request-scoped
// spans in tracing.go, logged separately). Named to match the spec's
// requested file name.
type TraceSpans struct {
	DocumentID       string    `json:"document_id"`
	TenantID         string    `json:"tenant_id"`
	IngestedAt       time.Time `json:"ingested_at"`
	ElapsedMs        float64   `json:"elapsed_ms"`
	UsedDaemon       bool      `json:"used_daemon"`
	TotalPages       int       `json:"total_pages"`
	PagesPassed      int       `json:"pages_passed"`
	PagesWarned      int       `json:"pages_warned"`
	PagesQuarantined int       `json:"pages_quarantined"`
	RedactClusters   int       `json:"redact_clusters"`
	ChunksEmitted    int       `json:"chunks_emitted"`
	TokensProcessed  int       `json:"tokens_processed"`
}

// IngestResult is everything one /ingest call needs to hand to the store.
// ChunksJSON/AuditJSON are the raw NDJSON records (already-formed JSON
// arrays) written verbatim, so chunks.json/audit_report.json always
// reflect exactly what the Rust core emitted rather than a Go-side
// re-serialization that could drift from it. StitchedMD is built
// separately from the chunk texts (see buildStitchedMarkdown in main.go).
type IngestResult struct {
	Spans         TraceSpans
	ChunksJSON    []byte
	AuditJSON     []byte
	StitchedMD    string
	SourcePDFPath string // temp file path to copy from; empty skips saving the PDF
}

// SaveIngestResult writes the full directory structure for one completed
// ingestion. Called once, synchronously, after an /ingest request finishes
// relaying its NDJSON response -- see main.go's handleIngest.
func (d *DocumentStore) SaveIngestResult(tenantID, documentID string, result IngestResult) error {
	dir := d.docDir(tenantID, documentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating document directory: %w", err)
	}

	if result.SourcePDFPath != "" {
		if err := copyFile(result.SourcePDFPath, filepath.Join(dir, "source.pdf")); err != nil {
			return fmt.Errorf("saving source.pdf: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "full_extracted.md"), []byte(result.StitchedMD), 0o644); err != nil {
		return fmt.Errorf("writing full_extracted.md: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chunks.json"), prettyJSON(result.ChunksJSON), 0o644); err != nil {
		return fmt.Errorf("writing chunks.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audit_report.json"), prettyJSON(result.AuditJSON), 0o644); err != nil {
		return fmt.Errorf("writing audit_report.json: %w", err)
	}
	spansJSON, err := json.MarshalIndent(result.Spans, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling trace_spans.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "trace_spans.json"), spansJSON, 0o644); err != nil {
		return fmt.Errorf("writing trace_spans.json: %w", err)
	}
	return nil
}

// prettyJSON re-indents an already-valid JSON byte slice for human
// readability on disk (part of the point of this being a plain directory
// instead of a database -- these files are meant to be `cat`-able).
// Falls back to a bare "[]" for empty input rather than writing a
// zero-byte file that would fail to parse as JSON at all.
func prettyJSON(data []byte) []byte {
	if len(data) == 0 {
		return []byte("[]")
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "", "  "); err != nil {
		return data
	}
	return buf.Bytes()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// DocumentSummary is one entry in GET /api/v1/documents.
type DocumentSummary struct {
	DocumentID       string    `json:"document_id"`
	IngestedAt       time.Time `json:"ingested_at"`
	TotalPages       int       `json:"total_pages"`
	ChunksEmitted    int       `json:"chunks_emitted"`
	PagesQuarantined int       `json:"pages_quarantined"`
	Status           string    `json:"status"` // "clean" | "needs_review"
}

// ListDocuments scans a tenant's document directories and returns a
// summary of each, newest first. Reads only trace_spans.json per
// document (small, already has everything a list view needs) rather than
// the potentially much larger chunks.json/audit_report.json.
func (d *DocumentStore) ListDocuments(tenantID string) ([]DocumentSummary, error) {
	tenantDir := filepath.Join(d.baseDir, "tenants", sanitizePathComponent(tenantID), "documents")
	entries, err := os.ReadDir(tenantDir)
	if os.IsNotExist(err) {
		return []DocumentSummary{}, nil
	}
	if err != nil {
		return nil, err
	}

	out := []DocumentSummary{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		spansPath := filepath.Join(tenantDir, entry.Name(), "trace_spans.json")
		data, err := os.ReadFile(spansPath)
		if err != nil {
			continue // a directory without a completed trace_spans.json isn't a finished document yet
		}
		var spans TraceSpans
		if err := json.Unmarshal(data, &spans); err != nil {
			continue
		}
		status := "clean"
		if spans.PagesQuarantined > 0 {
			status = "needs_review"
		}
		out = append(out, DocumentSummary{
			DocumentID:       spans.DocumentID,
			IngestedAt:       spans.IngestedAt,
			TotalPages:       spans.TotalPages,
			ChunksEmitted:    spans.ChunksEmitted,
			PagesQuarantined: spans.PagesQuarantined,
			Status:           status,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IngestedAt.After(out[j].IngestedAt) })
	return out, nil
}

// GetChunks returns the raw chunks.json bytes for one document, scoped to
// the given tenant (sanitizePathComponent means a cross-tenant ID can
// never resolve outside that tenant's own directory).
func (d *DocumentStore) GetChunks(tenantID, documentID string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.docDir(tenantID, documentID), "chunks.json"))
}

func (d *DocumentStore) GetAuditReport(tenantID, documentID string) ([]byte, error) {
	return os.ReadFile(filepath.Join(d.docDir(tenantID, documentID), "audit_report.json"))
}

func (d *DocumentStore) GetMarkdown(tenantID, documentID string) (string, error) {
	data, err := os.ReadFile(filepath.Join(d.docDir(tenantID, documentID), "full_extracted.md"))
	return string(data), err
}

func (d *DocumentStore) GetSourcePDFPath(tenantID, documentID string) (string, error) {
	path := filepath.Join(d.docDir(tenantID, documentID), "source.pdf")
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}
