// export_qdrant.go: connector for Qdrant (https://qdrant.tech), talking
// directly to its REST API (PUT /collections/{name}/points) with
// net/http + encoding/json -- no client SDK, which is why this one truly
// has zero extra Go dependencies, unlike pgvector.
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

type QdrantExporter struct {
	baseURL    string
	collection string
	apiKey     string
	dims       int
	client     *http.Client
}

func NewQdrantExporter(baseURL, collection, apiKey string, dims int) (*QdrantExporter, error) {
	if collection == "" {
		return nil, fmt.Errorf("qdrant collection name is required")
	}
	e := &QdrantExporter{
		baseURL:    strings.TrimRight(baseURL, "/"),
		collection: collection,
		apiKey:     apiKey,
		dims:       dims,
		client:     &http.Client{Timeout: 30 * time.Second},
	}
	if err := e.ensureCollection(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *QdrantExporter) Name() string { return "qdrant" }
func (e *QdrantExporter) Close() error { return nil } // stateless HTTP client, nothing to close

func (e *QdrantExporter) doRequest(method, path string, body interface{}) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("api-key", e.apiKey)
	}
	return e.client.Do(req)
}

// ensureCollection creates the target collection if it doesn't already
// exist. If dims is 0 (NullEmbedder / metadata-only export), Qdrant still
// requires *some* vector size to create a collection at all -- vectors are
// its whole reason for existing -- so we create a minimal 1-dimensional
// placeholder vector config in that case and document this clearly rather
// than silently picking an arbitrary "real" dimension that would be wrong
// for whatever embedder gets configured later.
func (e *QdrantExporter) ensureCollection() error {
	checkResp, err := e.doRequest(http.MethodGet, "/collections/"+e.collection, nil)
	if err != nil {
		return fmt.Errorf("checking collection: %w", err)
	}
	defer checkResp.Body.Close()
	if checkResp.StatusCode == http.StatusOK {
		return nil // already exists
	}

	dims := e.dims
	if dims <= 0 {
		dims = 1
	}
	createBody := map[string]interface{}{
		"vectors": map[string]interface{}{
			"size":     dims,
			"distance": "Cosine",
		},
	}
	resp, err := e.doRequest(http.MethodPut, "/collections/"+e.collection, createBody)
	if err != nil {
		return fmt.Errorf("creating collection: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("creating collection: qdrant returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

type qdrantPoint struct {
	ID      string                 `json:"id"`
	Vector  []float32              `json:"vector"`
	Payload map[string]interface{} `json:"payload,omitempty"`
}

func (e *QdrantExporter) Upsert(chunks []Chunk) error {
	points := make([]qdrantPoint, len(chunks))
	for i, c := range chunks {
		vec := c.Embedding
		if len(vec) == 0 {
			// Qdrant requires a vector on every point even in the
			// metadata-only case (see ensureCollection) -- a zero vector
			// is a clearly-inert placeholder, not a real embedding, and
			// similarity search against it is meaningless by construction
			// (which is the honest behavior: there IS no real vector yet).
			vec = make([]float32, 1)
		}
		payload := map[string]interface{}{"text": c.Text}
		for k, v := range c.Metadata {
			payload[k] = v
		}
		points[i] = qdrantPoint{ID: c.ID, Vector: vec, Payload: payload}
	}

	body := map[string]interface{}{"points": points}
	resp, err := e.doRequest(http.MethodPut, "/collections/"+e.collection+"/points?wait=true", body)
	if err != nil {
		return fmt.Errorf("upserting points: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("qdrant upsert returned %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
