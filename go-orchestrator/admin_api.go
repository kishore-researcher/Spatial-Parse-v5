// admin_api.go implements the Multi-Tenant Audit & Quarantine API:
//
//	GET  /api/v1/quarantine          list flagged pages for the caller's tenant
//	GET  /api/v1/quarantine/{id}     inspect one flagged page (full fidelity breakdown + markdown)
//	POST /api/v1/quarantine/{id}/approve   approve, optionally with edited markdown
//	POST /api/v1/quarantine/{id}/reject    reject (permanently excluded)
//
// Every handler here is wrapped in TenantAuth.requireTenant in main.go's
// route registration, so `tenantFromContext(r)` is always populated by the
// time these run.
package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

type AdminAPI struct {
	store *Store
}

func NewAdminAPI(store *Store) *AdminAPI {
	return &AdminAPI{store: store}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleList serves GET /api/v1/quarantine?status=pending&document_id=...
// status defaults to "pending" (the useful default for an admin inbox view)
// -- pass status=all to see approved/rejected history too.
func (a *AdminAPI) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	tenantID := tenantFromContext(r)
	status := ReviewStatus(r.URL.Query().Get("status"))
	if status == "" {
		status = StatusPending
	}
	if status == "all" {
		status = ""
	}
	documentID := r.URL.Query().Get("document_id")

	records := a.store.List(tenantID, status, documentID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"tenant_id": tenantID,
		"count":     len(records),
		"records":   records,
	})
}

// handleDetail serves GET /api/v1/quarantine/{id} -- the full record,
// including the c_retention/t_matrix breakdown of *why* the page failed,
// which is what lets an admin actually judge whether to approve or edit it
// rather than just seeing a bare score.
func (a *AdminAPI) handleDetail(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	tenantID := tenantFromContext(r)
	record, ok := a.store.Get(tenantID, id)
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

type reviewRequest struct {
	// ReviewedMarkdown is optional -- if omitted, approval uses the
	// original extracted markdown unchanged. Providing it is how an admin
	// corrects a page's text before it flows onward to export.
	ReviewedMarkdown string `json:"reviewed_markdown"`
	Note             string `json:"note"`
}

func (a *AdminAPI) handleApprove(w http.ResponseWriter, r *http.Request, id string) {
	a.handleReview(w, r, id, StatusApproved)
}

func (a *AdminAPI) handleReject(w http.ResponseWriter, r *http.Request, id string) {
	a.handleReview(w, r, id, StatusRejected)
}

func (a *AdminAPI) handleReview(w http.ResponseWriter, r *http.Request, id string, status ReviewStatus) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	tenantID := tenantFromContext(r)

	var body reviewRequest
	if r.Body != nil {
		// A body is optional (plain approve-as-is is a valid call with no
		// JSON body at all), so a decode error only matters if the body
		// was non-empty and actually malformed.
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&body); err != nil && err.Error() != "EOF" {
			http.Error(w, `{"error":"malformed JSON body"}`, http.StatusBadRequest)
			return
		}
	}

	record, err := a.store.Review(tenantID, id, status, body.ReviewedMarkdown, body.Note)
	if err != nil {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// Route dispatches /api/v1/quarantine and /api/v1/quarantine/{id}[/action]
// -- a small hand-rolled router rather than pulling in a routing library,
// consistent with the rest of this project's stdlib-only Go dependency
// policy (see README for why: this sandbox's network egress can't reach
// the Go module proxy or most vanity import domains, which was verified
// directly rather than assumed).
func (a *AdminAPI) Route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/quarantine")
	path = strings.Trim(path, "/")

	if path == "" {
		a.handleList(w, r)
		return
	}

	parts := strings.Split(path, "/")
	id := parts[0]
	if len(parts) == 1 {
		a.handleDetail(w, r, id)
		return
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "approve":
			a.handleApprove(w, r, id)
			return
		case "reject":
			a.handleReject(w, r, id)
			return
		}
	}
	http.NotFound(w, r)
}
