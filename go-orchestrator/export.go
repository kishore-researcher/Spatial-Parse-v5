// export.go: the shared contract behind all three "native database
// connector" implementations (pgvector, Qdrant, Pinecone).
//
// Important product-honesty note, not a footnote: a vector database
// upserts *vectors*. Nothing in this pipeline generates embeddings --
// the Rust core deliberately does zero-AI deterministic extraction, and
// that's a stated project goal, not an oversight. "Zero extra glue code"
// for the *database wiring* is true and delivered below (no client-side
// SDK required for Qdrant/Pinecone at all -- plain REST; pgvector via the
// one Go driver dependency this sandbox could actually fetch). But
// "zero extra glue code" cannot also mean "zero embedding model" -- that
// would require silently picking a paid third-party API and baking in
// an API key policy no one asked for. Instead: Embedder is a pluggable
// interface. NullEmbedder (metadata-only, no vector) is the safe default
// so the connectors are honest about doing nothing they weren't told to
// do. HTTPEmbedder targets the widely-adopted OpenAI-compatible
// /v1/embeddings request/response shape (implemented by OpenAI itself,
// and by self-hosted alternatives like LocalAI, vLLM, and Ollama's
// OpenAI-compat endpoint), so wiring in a real embedding provider is a
// URL and an API key, not new code.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Chunk is the common shape both normal pipeline chunks (from a completed
// /ingest run) and admin-approved quarantine records (Feature 1) are
// converted into before export -- a single interface serves both sources,
// per the design note in store.go's Approved() method.
type Chunk struct {
	ID        string                 `json:"id"`
	Text      string                 `json:"text"`
	Metadata  map[string]interface{} `json:"metadata"`
	Embedding []float32              `json:"embedding,omitempty"`
}

// Embedder turns chunk text into vectors. Implementations are expected to
// return a slice the same length as the input, in the same order.
type Embedder interface {
	Embed(texts []string) ([][]float32, error)
	Dimensions() int
}

// NullEmbedder is the default: no vectors, chunks exported as
// metadata-only records. This is a legitimate use case on its own (e.g.
// pgvector as a searchable document store queried by full-text search
// while vectors are backfilled separately), and it's what every exporter
// falls back to if no real embedder is configured -- so nothing silently
// pretends to have embeddings it doesn't.
type NullEmbedder struct{}

func (NullEmbedder) Embed(texts []string) ([][]float32, error) {
	return make([][]float32, len(texts)), nil
}
func (NullEmbedder) Dimensions() int { return 0 }

// HTTPEmbedder calls a generic OpenAI-compatible /v1/embeddings endpoint.
type HTTPEmbedder struct {
	URL    string
	APIKey string
	Model  string
	Dims   int
	Client *http.Client
}

func NewHTTPEmbedder(url, apiKey, model string, dims int) *HTTPEmbedder {
	return &HTTPEmbedder{URL: url, APIKey: apiKey, Model: model, Dims: dims, Client: &http.Client{Timeout: 30 * time.Second}}
}

func (e *HTTPEmbedder) Dimensions() int { return e.Dims }

type embeddingRequest struct {
	Input []string `json:"input"`
	Model string   `json:"model,omitempty"`
}
type embeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (e *HTTPEmbedder) Embed(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embeddingRequest{Input: texts, Model: e.Model})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.APIKey)
	}
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding endpoint returned %d: %s", resp.StatusCode, string(b))
	}
	var parsed embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make([][]float32, len(parsed.Data))
	for i, d := range parsed.Data {
		out[i] = d.Embedding
	}
	if len(out) != len(texts) {
		return nil, fmt.Errorf("embedding endpoint returned %d vectors for %d input texts", len(out), len(texts))
	}
	return out, nil
}

// Exporter is implemented by each database connector.
type Exporter interface {
	Name() string
	Upsert(chunks []Chunk) error
	Close() error
}

// EmbedChunks fills in each chunk's Embedding field via the given
// Embedder, batching all texts into a single call where the embedder
// supports it (every implementation above does). Chunks that already carry
// an embedding are left untouched, so a caller can mix pre-embedded and
// not-yet-embedded chunks in one export call.
func EmbedChunks(embedder Embedder, chunks []Chunk) error {
	var toEmbed []int
	var texts []string
	for i, c := range chunks {
		if len(c.Embedding) == 0 {
			toEmbed = append(toEmbed, i)
			texts = append(texts, c.Text)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	vectors, err := embedder.Embed(texts)
	if err != nil {
		return fmt.Errorf("embedding failed: %w", err)
	}
	for i, idx := range toEmbed {
		chunks[idx].Embedding = vectors[i]
	}
	return nil
}

// marshalMetadata renders a chunk's metadata map as JSON, defaulting to an
// empty object rather than SQL NULL / JSON null when nil, so downstream
// JSONB queries (e.g. `metadata->>'document_id'`) behave consistently
// whether or not the caller supplied any metadata.
func marshalMetadata(m map[string]interface{}) ([]byte, error) {
	if m == nil {
		m = map[string]interface{}{}
	}
	return json.Marshal(m)
}
