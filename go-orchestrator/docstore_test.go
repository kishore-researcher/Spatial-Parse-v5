package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestDocStore(t *testing.T) *DocumentStore {
	t.Helper()
	d, err := NewDocumentStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewDocumentStore failed: %v", err)
	}
	return d
}

// TestSanitizePathComponentBlocksTraversal is the most important test in
// this file: document_id arrives directly from client-controlled
// multipart form data, so this is a real security boundary, not a
// cosmetic cleanup.
func TestSanitizePathComponentBlocksTraversal(t *testing.T) {
	cases := []string{
		"../../../etc/passwd",
		"..",
		"../",
		"foo/../../bar",
		"/etc/passwd",
		"a/b/c",
		"..\\..\\windows",
	}
	for _, in := range cases {
		out := sanitizePathComponent(in)
		if strings.Contains(out, "..") {
			t.Errorf("sanitizePathComponent(%q) = %q still contains '..'", in, out)
		}
		if strings.ContainsAny(out, "/\\") {
			t.Errorf("sanitizePathComponent(%q) = %q still contains a path separator", in, out)
		}
	}
}

func TestSanitizePathComponentActuallyPreventsEscape(t *testing.T) {
	d := newTestDocStore(t)
	maliciousDocID := "../../../../../../tmp/escaped_pwned"
	dir := d.docDir("acme", maliciousDocID)
	if !strings.HasPrefix(dir, d.baseDir) {
		t.Fatalf("resolved directory %q escaped the store's base directory %q", dir, d.baseDir)
	}
}

func TestSaveAndListDocuments(t *testing.T) {
	d := newTestDocStore(t)
	result := IngestResult{
		Spans: TraceSpans{
			DocumentID: "doc1", TenantID: "acme", IngestedAt: time.Now().UTC(),
			TotalPages: 5, PagesPassed: 4, PagesQuarantined: 1, ChunksEmitted: 3,
		},
		ChunksJSON: []byte(`[{"chunk_id":0,"text":"hello"}]`),
		AuditJSON:  []byte(`[{"type":"page_fidelity"}]`),
		StitchedMD: "# Hello\n\nWorld",
	}
	if err := d.SaveIngestResult("acme", "doc1", result); err != nil {
		t.Fatalf("SaveIngestResult failed: %v", err)
	}

	docs, err := d.ListDocuments("acme")
	if err != nil {
		t.Fatalf("ListDocuments failed: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 document, got %d", len(docs))
	}
	if docs[0].Status != "needs_review" {
		t.Errorf("expected needs_review status (1 quarantined page), got %s", docs[0].Status)
	}
	if docs[0].TotalPages != 5 {
		t.Errorf("expected total_pages 5, got %d", docs[0].TotalPages)
	}
}

func TestListDocumentsIsolatesTenants(t *testing.T) {
	d := newTestDocStore(t)
	_ = d.SaveIngestResult("acme", "doc1", IngestResult{Spans: TraceSpans{DocumentID: "doc1", IngestedAt: time.Now()}})
	_ = d.SaveIngestResult("globex", "doc2", IngestResult{Spans: TraceSpans{DocumentID: "doc2", IngestedAt: time.Now()}})

	acmeDocs, _ := d.ListDocuments("acme")
	if len(acmeDocs) != 1 || acmeDocs[0].DocumentID != "doc1" {
		t.Fatalf("acme should only see doc1, got %v", acmeDocs)
	}
	globexDocs, _ := d.ListDocuments("globex")
	if len(globexDocs) != 1 || globexDocs[0].DocumentID != "doc2" {
		t.Fatalf("globex should only see doc2, got %v", globexDocs)
	}
}

func TestGetChunksMarkdownScopedToTenant(t *testing.T) {
	d := newTestDocStore(t)
	_ = d.SaveIngestResult("acme", "doc1", IngestResult{
		Spans:      TraceSpans{DocumentID: "doc1"},
		ChunksJSON: []byte(`[{"text":"acme chunk"}]`),
		StitchedMD: "acme markdown",
	})

	md, err := d.GetMarkdown("acme", "doc1")
	if err != nil || md != "acme markdown" {
		t.Fatalf("expected acme's own markdown, got %q, err=%v", md, err)
	}

	// globex asking for "doc1" resolves to ITS OWN (nonexistent) doc1
	// directory, never acme's -- this is the actual tenant-isolation
	// property, proven the same way store.go's is.
	if _, err := d.GetMarkdown("globex", "doc1"); err == nil {
		t.Fatal("globex should not be able to read acme's doc1 markdown")
	}
}

func TestListDocumentsEmptyForUnknownTenant(t *testing.T) {
	d := newTestDocStore(t)
	docs, err := d.ListDocuments("nobody-has-ingested-anything")
	if err != nil {
		t.Fatalf("unexpected error for a tenant with no documents yet: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("expected empty list, got %v", docs)
	}
}

func TestSaveIngestResultWritesAllFiles(t *testing.T) {
	d := newTestDocStore(t)
	src := filepath.Join(t.TempDir(), "upload.pdf")
	if err := os.WriteFile(src, []byte("%PDF-fake-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := IngestResult{
		Spans:         TraceSpans{DocumentID: "doc1"},
		ChunksJSON:    []byte(`[]`),
		AuditJSON:     []byte(`[]`),
		StitchedMD:    "content",
		SourcePDFPath: src,
	}
	if err := d.SaveIngestResult("acme", "doc1", result); err != nil {
		t.Fatalf("SaveIngestResult failed: %v", err)
	}

	dir := d.docDir("acme", "doc1")
	for _, name := range []string{"source.pdf", "full_extracted.md", "chunks.json", "audit_report.json", "trace_spans.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
	pdfBytes, _ := os.ReadFile(filepath.Join(dir, "source.pdf"))
	if string(pdfBytes) != "%PDF-fake-content" {
		t.Errorf("source.pdf content mismatch: %q", pdfBytes)
	}
}
