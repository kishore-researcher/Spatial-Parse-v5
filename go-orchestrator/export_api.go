// export_api.go: POST /api/v1/export -- takes chunks (either supplied
// directly in the request, or pulled from a tenant's admin-approved
// quarantine records) and pushes them to a configured target database.
//
// This is intentionally a single endpoint parameterized by target/config
// rather than three separate endpoints, since the request shape (chunks
// in, upsert out) is identical across all three connectors -- only
// construction differs, which is exactly what the Exporter interface in
// export.go exists to isolate.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type exportRequest struct {
	Target string `json:"target"` // "pgvector" | "qdrant" | "pinecone"

	// Chunks supplied directly. Optional if IncludeApprovedQuarantine is used instead (or in addition).
	Chunks []Chunk `json:"chunks"`

	// IncludeApprovedQuarantine, if set, pulls every approved (Feature 1)
	// record for this tenant (optionally filtered to one document) and
	// appends them as chunks -- this is the "approved quarantine content
	// flows into the vector database" path described in Feature 1's spec.
	IncludeApprovedQuarantine bool   `json:"include_approved_quarantine"`
	DocumentID                string `json:"document_id"`

	// Embedder config. Omit entirely for NullEmbedder (metadata-only export).
	EmbedderURL   string `json:"embedder_url"`
	EmbedderKey   string `json:"embedder_key"`
	EmbedderModel string `json:"embedder_model"`
	EmbedderDims  int    `json:"embedder_dims"`

	// Target-specific connection config -- only the fields relevant to
	// the chosen target need to be set.
	PgDSN             string `json:"pg_dsn"`
	PgTable           string `json:"pg_table"`
	QdrantURL         string `json:"qdrant_url"`
	QdrantAPIKey      string `json:"qdrant_api_key"`
	QdrantCollection  string `json:"qdrant_collection"`
	PineconeHost      string `json:"pinecone_host"`
	PineconeAPIKey    string `json:"pinecone_api_key"`
	PineconeNamespace string `json:"pinecone_namespace"`
}

type ExportAPI struct {
	store *Store
}

func NewExportAPI(store *Store) *ExportAPI {
	return &ExportAPI{store: store}
}

func (a *ExportAPI) buildEmbedder(req exportRequest) Embedder {
	if req.EmbedderURL == "" {
		return NullEmbedder{}
	}
	return NewHTTPEmbedder(req.EmbedderURL, req.EmbedderKey, req.EmbedderModel, req.EmbedderDims)
}

func (a *ExportAPI) buildExporter(req exportRequest) (Exporter, error) {
	switch req.Target {
	case "pgvector":
		return NewPgVectorExporter(req.PgDSN, req.PgTable, req.EmbedderDims)
	case "qdrant":
		return NewQdrantExporter(req.QdrantURL, req.QdrantCollection, req.QdrantAPIKey, req.EmbedderDims)
	case "pinecone":
		return NewPineconeExporter(req.PineconeHost, req.PineconeAPIKey, req.PineconeNamespace)
	default:
		return nil, fmt.Errorf("unknown export target %q (expected pgvector, qdrant, or pinecone)", req.Target)
	}
}

func (a *ExportAPI) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	tenantID := tenantFromContext(r)

	var req exportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"malformed request body: %s"}`, err), http.StatusBadRequest)
		return
	}

	chunks := append([]Chunk(nil), req.Chunks...)
	if req.IncludeApprovedQuarantine {
		for _, rec := range a.store.Approved(tenantID, req.DocumentID) {
			chunks = append(chunks, Chunk{
				ID:   rec.ID,
				Text: rec.FinalText(),
				Metadata: map[string]interface{}{
					"tenant_id":   rec.TenantID,
					"document_id": rec.DocumentID,
					"page_num":    rec.PageNum,
					"source":      "approved_quarantine",
					"s_fidelity":  rec.SFidelity,
				},
			})
		}
	}
	if len(chunks) == 0 {
		http.Error(w, `{"error":"no chunks to export (supply 'chunks' and/or set include_approved_quarantine)"}`, http.StatusBadRequest)
		return
	}

	embedder := a.buildEmbedder(req)
	if err := EmbedChunks(embedder, chunks); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
		return
	}

	exporter, err := a.buildExporter(req)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	defer exporter.Close()

	if err := exporter.Upsert(chunks); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"export to %s failed: %s"}`, exporter.Name(), err.Error()), http.StatusBadGateway)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"target":          exporter.Name(),
		"chunks_exported": len(chunks),
		"embedder":        fmt.Sprintf("%T", embedder),
	})
}
