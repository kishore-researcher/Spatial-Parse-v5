// export_pgvector.go: connector for Postgres + the pgvector extension.
//
// Uses database/sql + github.com/lib/pq (a pure-Go wire-protocol driver --
// no cgo, minimal transitive dependencies) rather than the more common
// jackc/pgx, specifically because lib/pq was the one Postgres driver this
// sandbox's network egress could actually fetch (verified directly: pgx's
// own dependency tree pulls in packages hosted on golang.org/x/*, which is
// blocked here the same way go.opentelemetry.io is -- see metrics.go). On
// unrestricted infrastructure, pgx is the more actively maintained choice
// and a reasonable thing to switch to; the Exporter interface in export.go
// makes that a contained change.
package main

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	_ "github.com/lib/pq"
)

type PgVectorExporter struct {
	db     *sql.DB
	table  string
	hasVec bool // whether the embedding column exists (dims > 0 at construction)
	dims   int
}

// NewPgVectorExporter connects, ensures the pgvector extension and target
// table exist, and returns a ready-to-use exporter. dims of 0 means
// "metadata-only" -- no embedding column is created, matching NullEmbedder.
func NewPgVectorExporter(dsn, table string, dims int) (*PgVectorExporter, error) {
	if table == "" {
		table = "rag_chunks"
	}
	if !isValidIdentifier(table) {
		return nil, fmt.Errorf("invalid table name %q", table)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}

	e := &PgVectorExporter{db: db, table: table, dims: dims, hasVec: dims > 0}

	if e.hasVec {
		if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
			db.Close()
			return nil, fmt.Errorf("enabling pgvector extension (is it installed on the server?): %w", err)
		}
		ddl := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s (
				id TEXT PRIMARY KEY,
				text TEXT NOT NULL,
				metadata JSONB,
				embedding VECTOR(%d)
			)`, quoteIdent(table), dims)
		if _, err := db.Exec(ddl); err != nil {
			db.Close()
			return nil, fmt.Errorf("creating table: %w", err)
		}
	} else {
		ddl := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s (
				id TEXT PRIMARY KEY,
				text TEXT NOT NULL,
				metadata JSONB
			)`, quoteIdent(table))
		if _, err := db.Exec(ddl); err != nil {
			db.Close()
			return nil, fmt.Errorf("creating table: %w", err)
		}
	}

	return e, nil
}

func (e *PgVectorExporter) Name() string { return "pgvector" }

func (e *PgVectorExporter) Close() error { return e.db.Close() }

// Upsert writes each chunk with INSERT ... ON CONFLICT (id) DO UPDATE, so
// re-exporting the same document (e.g. after an admin edits a quarantined
// page and re-approves it) overwrites rather than duplicates.
func (e *PgVectorExporter) Upsert(chunks []Chunk) error {
	tx, err := e.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op if Commit succeeds

	for _, c := range chunks {
		metaJSON, err := marshalMetadata(c.Metadata)
		if err != nil {
			return fmt.Errorf("chunk %s: marshaling metadata: %w", c.ID, err)
		}

		if e.hasVec {
			if len(c.Embedding) != 0 && len(c.Embedding) != e.dims {
				return fmt.Errorf("chunk %s: embedding has %d dimensions, table expects %d", c.ID, len(c.Embedding), e.dims)
			}
			var vecLiteral interface{}
			if len(c.Embedding) > 0 {
				vecLiteral = vectorLiteral(c.Embedding)
			} // else leave NULL -- a chunk exported before an embedder was configured

			q := fmt.Sprintf(`
				INSERT INTO %s (id, text, metadata, embedding)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (id) DO UPDATE SET text = EXCLUDED.text, metadata = EXCLUDED.metadata, embedding = EXCLUDED.embedding
			`, quoteIdent(e.table))
			if _, err := tx.Exec(q, c.ID, c.Text, metaJSON, vecLiteral); err != nil {
				return fmt.Errorf("chunk %s: %w", c.ID, err)
			}
		} else {
			q := fmt.Sprintf(`
				INSERT INTO %s (id, text, metadata)
				VALUES ($1, $2, $3)
				ON CONFLICT (id) DO UPDATE SET text = EXCLUDED.text, metadata = EXCLUDED.metadata
			`, quoteIdent(e.table))
			if _, err := tx.Exec(q, c.ID, c.Text, metaJSON); err != nil {
				return fmt.Errorf("chunk %s: %w", c.ID, err)
			}
		}
	}
	return tx.Commit()
}

// vectorLiteral formats a float32 slice as pgvector's text input syntax:
// '[0.1,0.2,0.3]'.
func vectorLiteral(v []float32) string {
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = strconv.FormatFloat(float64(f), 'f', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func isValidIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// quoteIdent double-quotes a table identifier already validated by
// isValidIdentifier -- defense in depth against SQL injection via a
// configured table name, even though it can only reach this code path via
// server configuration, not untrusted request input.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
