// Package main: store.go implements the persistence layer behind the
// Multi-Tenant Audit & Quarantine API.
//
// Deliberate scope choice: this is a file-backed JSON store (stdlib only:
// encoding/json + os + sync.RWMutex), not a real database. That's the right
// default for what was asked for -- "a light admin API endpoint" -- and it
// keeps the admin API deployable with zero external infrastructure. It is
// NOT what you'd want for a high-volume production deployment (no
// concurrent-writer transaction safety across processes, no indexing
// beyond what's implemented by hand below, full-file rewrite on every
// write). The upgrade path is straightforward: `Store` is accessed only
// through the methods below, so swapping the backing implementation for
// Postgres/SQLite later doesn't require touching any caller.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"
)

// ReviewStatus is the lifecycle state of a quarantined page awaiting
// admin action.
type ReviewStatus string

const (
	StatusPending  ReviewStatus = "pending"
	StatusApproved ReviewStatus = "approved"
	StatusRejected ReviewStatus = "rejected"
)

// PageRecord is one quarantined page persisted for admin review. Only
// pages the Rust core actually quarantines (S_fidelity < 0.70) are stored
// here -- passed and warned pages already flow through the normal chunk
// pipeline and don't need a review workflow.
type PageRecord struct {
	ID               string       `json:"id"`
	TenantID         string       `json:"tenant_id"`
	DocumentID       string       `json:"document_id"`
	PageNum          int          `json:"page_num"`
	SFidelity        float64      `json:"s_fidelity"`
	CRetention       float64      `json:"c_retention"`
	TMatrix          float64      `json:"t_matrix"`
	TableDetectedPDF bool         `json:"table_detected_in_pdf"`
	TableDetectedMD  bool         `json:"table_detected_in_md"`
	Markdown         string       `json:"markdown"`
	Status           ReviewStatus `json:"status"`
	ReviewedMarkdown string       `json:"reviewed_markdown,omitempty"`
	ReviewNote       string       `json:"review_note,omitempty"`
	ReviewedAt       *time.Time   `json:"reviewed_at,omitempty"`
	CreatedAt        time.Time    `json:"created_at"`
}

// FinalText returns the text that should flow onward to export (Feature 3)
// once a record is approved: the admin's edited version if they provided
// one, otherwise the original extracted markdown as-is.
func (p *PageRecord) FinalText() string {
	if p.ReviewedMarkdown != "" {
		return p.ReviewedMarkdown
	}
	return p.Markdown
}

// Store is a thread-safe, file-backed collection of PageRecords, isolated
// per tenant at every read path -- there is no method that returns records
// across tenants, which is what makes the tenant boundary real rather than
// just a filter an individual handler might forget to apply.
type Store struct {
	mu      sync.RWMutex
	path    string
	records map[string]*PageRecord // id -> record
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path, records: make(map[string]*PageRecord)}
	if err := s.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	var records []*PageRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range records {
		s.records[r.ID] = r
	}
	return nil
}

// saveLocked persists the full record set. Caller must hold s.mu (read or
// write lock -- this only reads s.records). Whole-file rewrite is
// deliberately simple and correct for the expected record volume (flagged
// pages across a modest number of documents, not a high-write-rate table).
func (s *Store) saveLocked() error {
	records := make([]*PageRecord, 0, len(s.records))
	for _, r := range s.records {
		records = append(records, r)
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path) // atomic on the same filesystem -- avoids a torn file on crash mid-write
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Add persists a new quarantined-page record and returns its assigned ID.
func (s *Store) Add(rec PageRecord) (string, error) {
	rec.ID = newID()
	rec.Status = StatusPending
	rec.CreatedAt = time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[rec.ID] = &rec
	if err := s.saveLocked(); err != nil {
		delete(s.records, rec.ID) // roll back the in-memory add if persistence failed
		return "", err
	}
	return rec.ID, nil
}

// List returns every record for a tenant, optionally filtered by status
// and/or document ID. Never returns another tenant's records -- tenantID
// is a required parameter, not an optional filter, specifically so a
// handler can't accidentally omit it.
func (s *Store) List(tenantID string, status ReviewStatus, documentID string) []*PageRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*PageRecord, 0)
	for _, r := range s.records {
		if r.TenantID != tenantID {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		if documentID != "" && r.DocumentID != documentID {
			continue
		}
		out = append(out, r)
	}
	return out
}

// Get returns a single record, but ONLY if it belongs to the given tenant
// -- a record ID from tenant A is invisible to tenant B even if they guess
// or otherwise obtain the ID, which is the actual security property a
// "multi-tenant" API needs to hold.
func (s *Store) Get(tenantID, id string) (*PageRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.records[id]
	if !ok || r.TenantID != tenantID {
		return nil, false
	}
	return r, true
}

// Review applies an approve/reject decision, scoped to the owning tenant
// the same way Get is.
func (s *Store) Review(tenantID, id string, status ReviewStatus, reviewedMarkdown, note string) (*PageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok || r.TenantID != tenantID {
		return nil, errors.New("record not found")
	}
	r.Status = status
	r.ReviewedMarkdown = reviewedMarkdown
	r.ReviewNote = note
	now := time.Now().UTC()
	r.ReviewedAt = &now
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	return r, nil
}

// ApprovedSince returns every approved record for a tenant -- this is what
// Feature 3's export worker pulls from to send admin-approved quarantine
// content onward alongside normally-chunked content.
func (s *Store) Approved(tenantID, documentID string) []*PageRecord {
	return s.List(tenantID, StatusApproved, documentID)
}
