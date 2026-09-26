// export_pinecone.go: connector for Pinecone (https://pinecone.io), via
// its REST API (POST /vectors/upsert against a per-index host) with plain
// net/http + encoding/json -- no client SDK required, same rationale as
// export_qdrant.go.
//
// Pinecone's REST API is host-per-index (you get a unique hostname per
// index from their console/API, unlike Qdrant's single base URL plus
// collection-name path segment), so the exporter takes that host directly
// rather than composing a URL from an index name.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type PineconeExporter struct {
	host      string
	apiKey    string
	namespace string
	client    *http.Client
}

func NewPineconeExporter(host, apiKey, namespace string) (*PineconeExporter, error) {
	if host == "" {
		return nil, fmt.Errorf("pinecone index host is required (from the Pinecone console: Index -> Host)")
	}
	if apiKey == "" {
		return nil, fmt.Errorf("pinecone API key is required")
	}
	return &PineconeExporter{
		host:      "https://" + strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://"),
		apiKey:    apiKey,
		namespace: namespace,
		client:    &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (e *PineconeExporter) Name() string { return "pinecone" }
func (e *PineconeExporter) Close() error { return nil }

type pineconeVector struct {
	ID       string                 `json:"id"`
	Values   []float32              `json:"values"`
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// Upsert pushes chunks to Pinecone. Pinecone's upsert API requires
// `values` on every vector -- there's no metadata-only mode the way
// pgvector's nullable column allows -- so the same zero-vector placeholder
// convention as Qdrant applies here too, documented for the same reason:
// honest about not having a real embedding yet, not a claim that
// similarity search is meaningful without one.
func (e *PineconeExporter) Upsert(chunks []Chunk) error {
	vectors := make([]pineconeVector, len(chunks))
	for i, c := range chunks {
		vals := c.Embedding
		if len(vals) == 0 {
			vals = make([]float32, 1)
		}
		meta := map[string]interface{}{"text": c.Text}
		for k, v := range c.Metadata {
			meta[k] = v
		}
		vectors[i] = pineconeVector{ID: c.ID, Values: vals, Metadata: meta}
	}

	body := map[string]interface{}{"vectors": vectors}
	if e.namespace != "" {
		body["namespace"] = e.namespace
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, e.host+"/vectors/upsert", bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Api-Key", e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("upserting vectors: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pinecone upsert returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
