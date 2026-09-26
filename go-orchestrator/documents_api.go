// documents_api.go implements the read side of Track D's storage layer:
//
//	GET /api/v1/documents                       list ingested documents for the caller's tenant
//	GET /api/v1/documents/{document_id}/chunks   the emitted chunk array + AST parent hierarchies
//	GET /api/v1/documents/{document_id}/markdown the full stitched markdown
//
// All three are wrapped in TenantAuth.requireTenant in main.go's route
// registration, same as the quarantine API.
package main

import (
	"net/http"
	"strings"
)

type DocumentsAPI struct {
	docs *DocumentStore
}

func NewDocumentsAPI(docs *DocumentStore) *DocumentsAPI {
	return &DocumentsAPI{docs: docs}
}

func (a *DocumentsAPI) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	tenantID := tenantFromContext(r)
	docs, err := a.docs.ListDocuments(tenantID)
	if err != nil {
		http.Error(w, `{"error":"failed to list documents"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tenant_id": tenantID,
		"count":     len(docs),
		"documents": docs,
	})
}

func (a *DocumentsAPI) handleChunks(w http.ResponseWriter, r *http.Request, documentID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	tenantID := tenantFromContext(r)
	data, err := a.docs.GetChunks(tenantID, documentID)
	if err != nil {
		http.Error(w, `{"error":"document not found, or has no chunks (it may be fully quarantined)"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (a *DocumentsAPI) handleMarkdown(w http.ResponseWriter, r *http.Request, documentID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	tenantID := tenantFromContext(r)
	md, err := a.docs.GetMarkdown(tenantID, documentID)
	if err != nil {
		http.Error(w, `{"error":"document not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(md))
}

// Route dispatches /api/v1/documents and /api/v1/documents/{id}/{chunks|markdown}
// -- the same small hand-rolled router style as admin_api.go, for the same
// stdlib-only reason (see admin_api.go's Route doc comment).
func (a *DocumentsAPI) Route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/documents")
	path = strings.Trim(path, "/")

	if path == "" {
		a.handleList(w, r)
		return
	}

	parts := strings.Split(path, "/")
	documentID := parts[0]
	if len(parts) == 2 {
		switch parts[1] {
		case "chunks":
			a.handleChunks(w, r, documentID)
			return
		case "markdown":
			a.handleMarkdown(w, r, documentID)
			return
		}
	}
	http.NotFound(w, r)
}
