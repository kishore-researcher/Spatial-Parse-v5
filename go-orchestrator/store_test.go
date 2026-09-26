package main

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "store.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	return s
}

func TestStoreAddAndGet(t *testing.T) {
	s := newTestStore(t)
	id, err := s.Add(PageRecord{TenantID: "acme", DocumentID: "doc1", PageNum: 13, SFidelity: 0.4, Markdown: "garbled text"})
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	rec, ok := s.Get("acme", id)
	if !ok {
		t.Fatal("expected to find the record just added")
	}
	if rec.Status != StatusPending {
		t.Errorf("expected new record to default to pending, got %s", rec.Status)
	}
	if rec.PageNum != 13 {
		t.Errorf("expected page_num 13, got %d", rec.PageNum)
	}
}

// TestStoreTenantIsolation is the single most important test in this file:
// it's the actual security property "multi-tenant" claims to provide.
func TestStoreTenantIsolation(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.Add(PageRecord{TenantID: "acme", DocumentID: "doc1", PageNum: 1})

	if _, ok := s.Get("globex", id); ok {
		t.Fatal("globex should NOT be able to Get acme's record by ID")
	}
	if _, ok := s.Get("acme", id); !ok {
		t.Fatal("acme should be able to Get its own record")
	}

	globexList := s.List("globex", "", "")
	if len(globexList) != 0 {
		t.Fatalf("globex's List should be empty, got %d records", len(globexList))
	}
	acmeList := s.List("acme", "", "")
	if len(acmeList) != 1 {
		t.Fatalf("acme's List should have 1 record, got %d", len(acmeList))
	}

	if _, err := s.Review("globex", id, StatusApproved, "", ""); err == nil {
		t.Fatal("globex should NOT be able to approve acme's record")
	}
	if _, err := s.Review("acme", id, StatusApproved, "", ""); err != nil {
		t.Fatalf("acme should be able to approve its own record: %v", err)
	}
}

func TestStoreListFiltersByStatusAndDocument(t *testing.T) {
	s := newTestStore(t)
	id1, _ := s.Add(PageRecord{TenantID: "acme", DocumentID: "doc1", PageNum: 1})
	_, _ = s.Add(PageRecord{TenantID: "acme", DocumentID: "doc2", PageNum: 5})
	_, _ = s.Review("acme", id1, StatusApproved, "", "")

	pending := s.List("acme", StatusPending, "")
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending record, got %d", len(pending))
	}
	approved := s.List("acme", StatusApproved, "")
	if len(approved) != 1 || approved[0].DocumentID != "doc1" {
		t.Fatalf("expected 1 approved record for doc1, got %v", approved)
	}
	byDoc := s.List("acme", "", "doc2")
	if len(byDoc) != 1 || byDoc[0].PageNum != 5 {
		t.Fatalf("expected doc2's single record, got %v", byDoc)
	}
}

func TestStoreReviewWithEditedMarkdownAffectsFinalText(t *testing.T) {
	s := newTestStore(t)
	id, _ := s.Add(PageRecord{TenantID: "acme", DocumentID: "doc1", PageNum: 1, Markdown: "original garbled text"})

	rec, _ := s.Get("acme", id)
	if rec.FinalText() != "original garbled text" {
		t.Fatalf("before review, FinalText should be the original markdown, got %q", rec.FinalText())
	}

	_, err := s.Review("acme", id, StatusApproved, "corrected text", "fixed OCR noise")
	if err != nil {
		t.Fatalf("Review failed: %v", err)
	}
	rec, _ = s.Get("acme", id)
	if rec.FinalText() != "corrected text" {
		t.Fatalf("after review with an edit, FinalText should be the edited version, got %q", rec.FinalText())
	}
	if rec.Status != StatusApproved {
		t.Errorf("expected status approved, got %s", rec.Status)
	}
}

func TestStoreApprovedHelper(t *testing.T) {
	s := newTestStore(t)
	id1, _ := s.Add(PageRecord{TenantID: "acme", DocumentID: "doc1", PageNum: 1})
	id2, _ := s.Add(PageRecord{TenantID: "acme", DocumentID: "doc1", PageNum: 2})
	_, _ = s.Add(PageRecord{TenantID: "acme", DocumentID: "doc2", PageNum: 1}) // different document, left pending

	_, _ = s.Review("acme", id1, StatusApproved, "", "")
	_, _ = s.Review("acme", id2, StatusRejected, "", "")

	approved := s.Approved("acme", "doc1")
	if len(approved) != 1 || approved[0].ID != id1 {
		t.Fatalf("expected exactly the one approved record for doc1, got %v", approved)
	}
}

func TestStorePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	s1, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	id, _ := s1.Add(PageRecord{TenantID: "acme", DocumentID: "doc1", PageNum: 1, Markdown: "hello"})

	// Simulate a process restart: open a fresh Store against the same file.
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reopening store failed: %v", err)
	}
	rec, ok := s2.Get("acme", id)
	if !ok {
		t.Fatal("expected record to survive a reload from disk")
	}
	if rec.Markdown != "hello" {
		t.Errorf("expected markdown to survive reload, got %q", rec.Markdown)
	}
}

func TestNewStoreOnMissingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does_not_exist_yet.json")
	if _, err := os.Stat(path); err == nil {
		t.Fatal("test setup error: file should not exist")
	}
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore should tolerate a missing file, got error: %v", err)
	}
	if len(s.List("anyone", "", "")) != 0 {
		t.Fatal("expected an empty store")
	}
}
