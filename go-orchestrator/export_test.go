package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestNullEmbedder(t *testing.T) {
	e := NullEmbedder{}
	vecs, err := e.Embed([]string{"a", "b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vecs) != 2 || vecs[0] != nil || vecs[1] != nil {
		t.Fatalf("expected 2 nil vectors, got %v", vecs)
	}
	if e.Dimensions() != 0 {
		t.Fatalf("expected 0 dimensions, got %d", e.Dimensions())
	}
}

func TestHTTPEmbedder(t *testing.T) {
	var receivedBody embeddingRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer test-key auth header, got %q", r.Header.Get("Authorization"))
		}
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		resp := embeddingResponse{}
		for range receivedBody.Input {
			resp.Data = append(resp.Data, struct {
				Embedding []float32 `json:"embedding"`
			}{Embedding: []float32{0.1, 0.2, 0.3}})
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, "test-key", "text-embedding-3-small", 3)
	vecs, err := e.Embed([]string{"hello", "world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(vecs))
	}
	if len(vecs[0]) != 3 {
		t.Fatalf("expected 3-dim vector, got %d", len(vecs[0]))
	}
	if len(receivedBody.Input) != 2 || receivedBody.Input[0] != "hello" {
		t.Fatalf("embedder didn't send expected input texts: %v", receivedBody.Input)
	}
}

func TestHTTPEmbedderMismatchedResponseLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately return only 1 embedding for 2 input texts.
		json.NewEncoder(w).Encode(embeddingResponse{Data: []struct {
			Embedding []float32 `json:"embedding"`
		}{{Embedding: []float32{0.1}}}})
	}))
	defer srv.Close()

	e := NewHTTPEmbedder(srv.URL, "", "", 1)
	_, err := e.Embed([]string{"a", "b"})
	if err == nil {
		t.Fatal("expected an error for mismatched response length, got nil")
	}
}

func TestEmbedChunksSkipsAlreadyEmbedded(t *testing.T) {
	calls := 0
	fake := fakeEmbedder{fn: func(texts []string) ([][]float32, error) {
		calls++
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 2, 3}
		}
		return out, nil
	}}
	chunks := []Chunk{
		{ID: "a", Text: "already has one", Embedding: []float32{9, 9, 9}},
		{ID: "b", Text: "needs one"},
	}
	if err := EmbedChunks(fake, chunks); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 embed call (batched), got %d", calls)
	}
	if chunks[0].Embedding[0] != 9 {
		t.Fatalf("pre-existing embedding was overwritten: %v", chunks[0].Embedding)
	}
	if len(chunks[1].Embedding) != 3 {
		t.Fatalf("chunk missing embedding was not filled: %v", chunks[1].Embedding)
	}
}

type fakeEmbedder struct {
	fn func([]string) ([][]float32, error)
}

func (f fakeEmbedder) Embed(texts []string) ([][]float32, error) { return f.fn(texts) }
func (f fakeEmbedder) Dimensions() int                           { return 3 }

// --- Qdrant: mocked REST server ---

func TestQdrantExporterCreatesCollectionAndUpserts(t *testing.T) {
	var sawCreate, sawUpsert bool
	var upsertBody map[string]interface{}

	mux := http.NewServeMux()
	mux.HandleFunc("/collections/test_collection", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusNotFound) // doesn't exist yet -- forces creation
		case http.MethodPut:
			sawCreate = true
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			vectors, _ := body["vectors"].(map[string]interface{})
			if vectors["size"].(float64) != 4 {
				t.Errorf("expected collection created with size 4, got %v", vectors["size"])
			}
			w.WriteHeader(http.StatusOK)
		}
	})
	mux.HandleFunc("/collections/test_collection/points", func(w http.ResponseWriter, r *http.Request) {
		sawUpsert = true
		if r.Header.Get("api-key") != "qdrant-secret" {
			t.Errorf("expected api-key header, got %q", r.Header.Get("api-key"))
		}
		json.NewDecoder(r.Body).Decode(&upsertBody)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	exporter, err := NewQdrantExporter(srv.URL, "test_collection", "qdrant-secret", 4)
	if err != nil {
		t.Fatalf("unexpected error creating exporter: %v", err)
	}
	if !sawCreate {
		t.Fatal("expected collection creation request, didn't see one")
	}

	chunks := []Chunk{
		{ID: "p1", Text: "hello", Embedding: []float32{0.1, 0.2, 0.3, 0.4}, Metadata: map[string]interface{}{"doc": "d1"}},
	}
	if err := exporter.Upsert(chunks); err != nil {
		t.Fatalf("unexpected upsert error: %v", err)
	}
	if !sawUpsert {
		t.Fatal("expected upsert request, didn't see one")
	}
	points, ok := upsertBody["points"].([]interface{})
	if !ok || len(points) != 1 {
		t.Fatalf("expected 1 point in upsert body, got %v", upsertBody)
	}
	point := points[0].(map[string]interface{})
	if point["id"] != "p1" {
		t.Errorf("expected point id p1, got %v", point["id"])
	}
	payload := point["payload"].(map[string]interface{})
	if payload["text"] != "hello" || payload["doc"] != "d1" {
		t.Errorf("payload missing expected fields: %v", payload)
	}
}

func TestQdrantExporterMetadataOnlyUsesPlaceholderVector(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/collections/mo", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK) // already exists -- skip creation entirely
		}
	})
	var upsertBody map[string]interface{}
	mux.HandleFunc("/collections/mo/points", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&upsertBody)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	exporter, err := NewQdrantExporter(srv.URL, "mo", "", 0) // dims=0 -> metadata-only
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := exporter.Upsert([]Chunk{{ID: "x", Text: "no embedding here"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	points := upsertBody["points"].([]interface{})
	point := points[0].(map[string]interface{})
	vec := point["vector"].([]interface{})
	if len(vec) != 1 {
		t.Fatalf("expected a 1-dim placeholder vector for metadata-only export, got %v", vec)
	}
}

// --- Pinecone: mocked REST server ---

func TestPineconeExporterUpsert(t *testing.T) {
	var receivedBody map[string]interface{}
	var sawAuth string
	// PineconeExporter deliberately always forces https:// (real Pinecone
	// is HTTPS-only, and silently allowing plain HTTP would risk sending
	// an API key in cleartext) -- so this test needs a TLS server, with
	// the client's cert verification relaxed to trust the test server's
	// self-signed certificate. That trade-off lives only in the test.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vectors/upsert" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		sawAuth = r.Header.Get("Api-Key")
		json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host := srv.URL[len("https://"):]
	exporter, err := NewPineconeExporter(host, "pinecone-secret", "prod-namespace")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	exporter.client = srv.Client() // trusts the test server's self-signed cert

	chunks := []Chunk{
		{ID: "v1", Text: "some chunk text", Embedding: []float32{0.5, 0.6}, Metadata: map[string]interface{}{"source": "test"}},
	}
	if err := exporter.Upsert(chunks); err != nil {
		t.Fatalf("unexpected upsert error: %v", err)
	}
	if sawAuth != "pinecone-secret" {
		t.Errorf("expected Api-Key header, got %q", sawAuth)
	}
	if receivedBody["namespace"] != "prod-namespace" {
		t.Errorf("expected namespace in body, got %v", receivedBody)
	}
	vectors := receivedBody["vectors"].([]interface{})
	v := vectors[0].(map[string]interface{})
	if v["id"] != "v1" {
		t.Errorf("expected vector id v1, got %v", v["id"])
	}
	meta := v["metadata"].(map[string]interface{})
	if meta["text"] != "some chunk text" || meta["source"] != "test" {
		t.Errorf("metadata missing expected fields: %v", meta)
	}
}

func TestPineconeExporterRequiresHostAndKey(t *testing.T) {
	if _, err := NewPineconeExporter("", "key", ""); err == nil {
		t.Fatal("expected error for missing host")
	}
	if _, err := NewPineconeExporter("host.example.com", "", ""); err == nil {
		t.Fatal("expected error for missing API key")
	}
}

// --- pgvector: real integration test against a live Postgres, skipped gracefully if unreachable ---

func TestPgVectorExporterIntegration(t *testing.T) {
	dsn := os.Getenv("TEST_PG_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:testpass@127.0.0.1/ragtest?sslmode=disable"
	}
	exporter, err := NewPgVectorExporter(dsn, "rag_chunks_go_test", 4)
	if err != nil {
		t.Skipf("skipping: no reachable Postgres for integration test (%v)", err)
	}
	defer exporter.Close()
	defer exporter.db.Exec(`DROP TABLE IF EXISTS rag_chunks_go_test`)

	chunks := []Chunk{
		{ID: "t1", Text: "first chunk", Embedding: []float32{0.1, 0.2, 0.3, 0.4}, Metadata: map[string]interface{}{"n": 1.0}},
		{ID: "t2", Text: "second chunk", Metadata: map[string]interface{}{"n": 2.0}}, // no embedding -- NULL column
	}
	if err := exporter.Upsert(chunks); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}

	// Re-upsert the same ID with different text to confirm ON CONFLICT
	// DO UPDATE actually overwrites rather than erroring or duplicating.
	chunks[0].Text = "first chunk (updated)"
	if err := exporter.Upsert(chunks[:1]); err != nil {
		t.Fatalf("re-upsert failed: %v", err)
	}

	var text string
	row := exporter.db.QueryRow(`SELECT text FROM rag_chunks_go_test WHERE id = 't1'`)
	if err := row.Scan(&text); err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if text != "first chunk (updated)" {
		t.Errorf("expected updated text, got %q", text)
	}

	var count int
	if err := exporter.db.QueryRow(`SELECT count(*) FROM rag_chunks_go_test`).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 rows (no duplication from re-upsert), got %d", count)
	}
}
