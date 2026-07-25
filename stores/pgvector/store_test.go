package pgvector

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestValidateIdentifier(t *testing.T) {
	valid := []string{"documents", "_private", "Doc1", "a", "embedding_v2", strings.Repeat("a", 63)}
	for _, name := range valid {
		if err := ValidateIdentifier(name); err != nil {
			t.Errorf("ValidateIdentifier(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"1documents",
		"doc uments",
		"doc-uments",
		"public.documents",
		`doc"uments`,
		"documents;DROP TABLE users",
		"documents--",
		"documents/*x*/",
		"docum\nents",
		"docüments",
		"'documents'",
		"*",
		strings.Repeat("a", 64),
	}
	for _, name := range invalid {
		err := ValidateIdentifier(name)
		if err == nil {
			t.Errorf("ValidateIdentifier(%q) = nil, want error", name)
			continue
		}
		if !errors.Is(err, ErrInvalidIdentifier) {
			t.Errorf("ValidateIdentifier(%q) error = %v, want ErrInvalidIdentifier", name, err)
		}
	}
}

func TestConfigValidateRejectsInjection(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"table", Config{Table: "docs; DROP TABLE users"}},
		{"content column", Config{ContentColumn: "content, (SELECT password FROM users)"}},
		{"embedding column", Config{EmbeddingColumn: "emb`"}},
		{"source column", Config{SourceColumn: "src'"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cfg.Validate(); !errors.Is(err, ErrInvalidIdentifier) {
				t.Fatalf("Validate() = %v, want ErrInvalidIdentifier", err)
			}
			if _, err := NewWithQuerier(&fakeQuerier{}, tt.cfg); err == nil {
				t.Fatal("NewWithQuerier accepted an invalid identifier")
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	s, err := NewWithQuerier(&fakeQuerier{}, Config{})
	if err != nil {
		t.Fatalf("NewWithQuerier: %v", err)
	}
	cfg := s.Config()
	if cfg.Table != DefaultTable || cfg.ContentColumn != DefaultContentColumn ||
		cfg.EmbeddingColumn != DefaultEmbeddingColumn || cfg.SourceColumn != DefaultSourceColumn {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.MaxTopK != DefaultMaxTopK {
		t.Fatalf("MaxTopK = %d, want %d", cfg.MaxTopK, DefaultMaxTopK)
	}
}

func TestQueryConstruction(t *testing.T) {
	s, err := NewWithQuerier(&fakeQuerier{}, Config{
		Table:           "kb_chunks",
		ContentColumn:   "body",
		EmbeddingColumn: "vec",
		SourceColumn:    "url",
	})
	if err != nil {
		t.Fatalf("NewWithQuerier: %v", err)
	}
	q := s.Query()

	for _, want := range []string{
		`SELECT "body"`,
		`COALESCE("url", '') AS source_url`,
		`1 - ("vec" <=> $1) AS similarity`,
		`FROM "kb_chunks"`,
		`ORDER BY "vec" <=> $1`,
		`LIMIT $2`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
	// Every identifier is quoted, and no literal value is ever interpolated.
	if strings.Count(q, "$1") != 2 || strings.Count(q, "$2") != 1 {
		t.Errorf("expected parameterized vector and limit, got %q", q)
	}
}

func TestNilPoolAndQuerier(t *testing.T) {
	if _, err := New(nil, Config{}); err == nil {
		t.Fatal("New(nil pool) = nil error, want error")
	}
	if _, err := NewWithQuerier(nil, Config{}); err == nil {
		t.Fatal("NewWithQuerier(nil) = nil error, want error")
	}
}

func TestFormatVector(t *testing.T) {
	got := FormatVector([]float32{0.5, -1, 0})
	if got != "[0.5,-1,0]" {
		t.Fatalf("FormatVector = %q", got)
	}
	if FormatVector(nil) != "[]" {
		t.Fatalf("FormatVector(nil) = %q, want []", FormatVector(nil))
	}
}

func TestSearchBindsParameters(t *testing.T) {
	fq := &fakeQuerier{rows: &fakeRows{data: [][]any{
		{"chunk one", "https://a", 0.91},
		{"chunk two", "", 0.42},
	}}}
	s, err := NewWithQuerier(fq, Config{})
	if err != nil {
		t.Fatalf("NewWithQuerier: %v", err)
	}

	chunks, err := s.Search(context.Background(), []float32{1, 0, 0}, 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("len = %d, want 2", len(chunks))
	}
	if chunks[0].Content != "chunk one" || chunks[0].SourceURL != "https://a" || chunks[0].Similarity != 0.91 {
		t.Fatalf("chunk[0] = %+v", chunks[0])
	}
	if fq.sql != s.Query() {
		t.Fatalf("executed %q, want %q", fq.sql, s.Query())
	}
	if len(fq.args) != 2 {
		t.Fatalf("args = %v, want 2 bound parameters", fq.args)
	}
	if fq.args[0] != "[1,0,0]" {
		t.Fatalf("args[0] = %v, want the formatted vector", fq.args[0])
	}
	if fq.args[1] != 5 {
		t.Fatalf("args[1] = %v, want 5", fq.args[1])
	}
	if !fq.rows.closed {
		t.Fatal("rows were not closed")
	}
}

func TestSearchCapsTopK(t *testing.T) {
	fq := &fakeQuerier{rows: &fakeRows{}}
	s, err := NewWithQuerier(fq, Config{MaxTopK: 10})
	if err != nil {
		t.Fatalf("NewWithQuerier: %v", err)
	}
	if _, err := s.Search(context.Background(), []float32{1}, 1000); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if fq.args[1] != 10 {
		t.Fatalf("topK = %v, want capped to 10", fq.args[1])
	}
}

func TestSearchGuards(t *testing.T) {
	s, err := NewWithQuerier(&fakeQuerier{rows: &fakeRows{}}, Config{})
	if err != nil {
		t.Fatalf("NewWithQuerier: %v", err)
	}

	if got, err := s.Search(context.Background(), []float32{1}, 0); err != nil || got != nil {
		t.Fatalf("topK=0 => %v, %v; want nil, nil", got, err)
	}
	if _, err := s.Search(context.Background(), nil, 3); err == nil {
		t.Fatal("empty vector accepted")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Search(ctx, []float32{1}, 3); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestSearchPropagatesErrors(t *testing.T) {
	qErr := errors.New("connection refused")
	s, _ := NewWithQuerier(&fakeQuerier{err: qErr}, Config{})
	if _, err := s.Search(context.Background(), []float32{1}, 3); !errors.Is(err, qErr) {
		t.Fatalf("err = %v, want wrapped %v", err, qErr)
	}

	scanErr := errors.New("bad scan")
	s2, _ := NewWithQuerier(&fakeQuerier{rows: &fakeRows{data: [][]any{{"x", "", 0.1}}, scanErr: scanErr}}, Config{})
	if _, err := s2.Search(context.Background(), []float32{1}, 3); !errors.Is(err, scanErr) {
		t.Fatalf("err = %v, want wrapped %v", err, scanErr)
	}

	rowsErr := errors.New("stream broken")
	s3, _ := NewWithQuerier(&fakeQuerier{rows: &fakeRows{err: rowsErr}}, Config{})
	if _, err := s3.Search(context.Background(), []float32{1}, 3); !errors.Is(err, rowsErr) {
		t.Fatalf("err = %v, want wrapped %v", err, rowsErr)
	}
}

// --- test doubles -----------------------------------------------------------

type fakeQuerier struct {
	sql  string
	args []any
	rows *fakeRows
	err  error
}

func (f *fakeQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.sql = sql
	f.args = args
	if f.err != nil {
		return nil, f.err
	}
	if f.rows == nil {
		f.rows = &fakeRows{}
	}
	return f.rows, nil
}

type fakeRows struct {
	data    [][]any
	i       int
	closed  bool
	err     error
	scanErr error
}

func (r *fakeRows) Close()                                       { r.closed = true }
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

func (r *fakeRows) Next() bool {
	if r.i >= len(r.data) {
		return false
	}
	r.i++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.data[r.i-1]
	if len(dest) != len(row) {
		return errors.New("column count mismatch")
	}
	*(dest[0].(*string)) = row[0].(string)
	*(dest[1].(*string)) = row[1].(string)
	*(dest[2].(*float64)) = row[2].(float64)
	return nil
}
