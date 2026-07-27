package pgvector

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestIntegrationSearch runs against a real PostgreSQL with the pgvector
// extension. It is skipped unless TOKIPE_PGVECTOR_DSN is set, e.g.:
//
//	docker run -d --name tokipe-pgvector -p 5433:5432 \
//	  -e POSTGRES_PASSWORD=postgres pgvector/pgvector:pg16
//	TOKIPE_PGVECTOR_DSN='postgres://postgres:postgres@localhost:5433/postgres' \
//	  go test ./... -run TestIntegration -v
//
// The test creates and drops its own throwaway table.
func TestIntegrationSearch(t *testing.T) {
	dsn := os.Getenv("TOKIPE_PGVECTOR_DSN")
	if dsn == "" {
		t.Skip("TOKIPE_PGVECTOR_DSN not set; skipping pgvector integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, pool, err := Connect(ctx, dsn, Config{Table: "tokipe_it_chunks"})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	exec(`CREATE EXTENSION IF NOT EXISTS vector`)
	exec(`DROP TABLE IF EXISTS tokipe_it_chunks`)
	exec(`CREATE TABLE tokipe_it_chunks (
		id serial PRIMARY KEY,
		content text NOT NULL,
		source_url text,
		embedding vector(3)
	)`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS tokipe_it_chunks`)
	})

	exec(`INSERT INTO tokipe_it_chunks (content, source_url, embedding) VALUES
		($1, $2, $3), ($4, $5, $6)`,
		"near", "https://near", FormatVector([]float32{1, 0, 0}),
		"far", "https://far", FormatVector([]float32{0, 1, 0}))

	chunks, err := store.Search(ctx, []float32{1, 0, 0}, 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("len = %d, want 1", len(chunks))
	}
	if chunks[0].Content != "near" {
		t.Fatalf("content = %q, want %q", chunks[0].Content, "near")
	}
	if chunks[0].Similarity < 0.99 {
		t.Fatalf("similarity = %v, want ~1", chunks[0].Similarity)
	}
}
