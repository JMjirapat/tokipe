// Package pgvector implements stores.VectorStore on top of PostgreSQL with the
// pgvector extension, using jackc/pgx v5 and pgxpool.
//
// This is a separate Go module (see go.mod in this directory) so the agentkit
// core stays stdlib-only. Import it explicitly if you want it.
//
// SQL SAFETY: every value is bound as a query parameter — user input is never
// concatenated into SQL. Table and column names cannot be parameterized by the
// protocol, so configurable identifiers are validated against a strict
// allowlist (see ValidateIdentifier) and quoted before interpolation.
package pgvector

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/JMjirapat/tokipe/pipeline"
	"github.com/JMjirapat/tokipe/stores"
)

// identifierRE is the strict allowlist for any SQL identifier this package will
// interpolate: an unquoted-style ASCII identifier, nothing else.
var identifierRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// MaxIdentifierLen matches PostgreSQL's NAMEDATALEN-1 limit.
const MaxIdentifierLen = 63

// ErrInvalidIdentifier is returned when a configured table or column name is
// not a plain SQL identifier.
var ErrInvalidIdentifier = errors.New("pgvector: invalid SQL identifier")

// ValidateIdentifier reports whether name is safe to interpolate into SQL as an
// identifier. Only `^[a-zA-Z_][a-zA-Z0-9_]*$`, at most MaxIdentifierLen bytes,
// is accepted — no quotes, no dots, no whitespace, no unicode.
func ValidateIdentifier(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidIdentifier)
	}
	if len(name) > MaxIdentifierLen {
		return fmt.Errorf("%w: %q exceeds %d bytes", ErrInvalidIdentifier, name, MaxIdentifierLen)
	}
	if !identifierRE.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidIdentifier, name)
	}
	return nil
}

// quoteIdentifier double-quotes an already-validated identifier. Because
// ValidateIdentifier rejects `"` outright this cannot produce an escape, but
// quoting still protects against reserved words.
func quoteIdentifier(name string) string { return `"` + name + `"` }

// Config names the table and columns the store reads from. Zero values fall
// back to the defaults below.
type Config struct {
	Table           string // default "documents"
	ContentColumn   string // default "content"
	EmbeddingColumn string // default "embedding"
	SourceColumn    string // default "source_url"

	// MaxTopK caps the topK a caller may ask for. Zero means DefaultMaxTopK.
	MaxTopK int
}

// Defaults.
const (
	DefaultTable           = "documents"
	DefaultContentColumn   = "content"
	DefaultEmbeddingColumn = "embedding"
	DefaultSourceColumn    = "source_url"
	DefaultMaxTopK         = 100
)

func (c Config) withDefaults() Config {
	if c.Table == "" {
		c.Table = DefaultTable
	}
	if c.ContentColumn == "" {
		c.ContentColumn = DefaultContentColumn
	}
	if c.EmbeddingColumn == "" {
		c.EmbeddingColumn = DefaultEmbeddingColumn
	}
	if c.SourceColumn == "" {
		c.SourceColumn = DefaultSourceColumn
	}
	if c.MaxTopK <= 0 {
		c.MaxTopK = DefaultMaxTopK
	}
	return c
}

// Validate checks every configured identifier against the allowlist.
func (c Config) Validate() error {
	c = c.withDefaults()
	for label, name := range map[string]string{
		"table":            c.Table,
		"content column":   c.ContentColumn,
		"embedding column": c.EmbeddingColumn,
		"source column":    c.SourceColumn,
	} {
		if err := ValidateIdentifier(name); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return nil
}

// Querier is the subset of pgxpool.Pool this store needs. It exists so the
// query-construction logic is unit-testable without a live database.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store is a stores.VectorStore backed by pgvector.
type Store struct {
	db    Querier
	cfg   Config
	query string
}

var _ stores.VectorStore = (*Store)(nil)

// New builds a Store over an existing pool. It returns an error if any
// configured identifier fails validation.
func New(pool *pgxpool.Pool, cfg Config) (*Store, error) {
	if pool == nil {
		return nil, errors.New("pgvector: nil pool")
	}
	return NewWithQuerier(pool, cfg)
}

// NewWithQuerier is New for any Querier (a pool, a conn, or a test double).
func NewWithQuerier(db Querier, cfg Config) (*Store, error) {
	if db == nil {
		return nil, errors.New("pgvector: nil querier")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	return &Store{db: db, cfg: cfg, query: buildSearchQuery(cfg)}, nil
}

// Connect opens a new pool from a connection string and builds a Store. The
// caller owns the returned pool and must Close it.
func Connect(ctx context.Context, dsn string, cfg Config) (*Store, *pgxpool.Pool, error) {
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("pgvector: connect: %w", err)
	}
	store, err := New(pool, cfg)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return store, pool, nil
}

// Query returns the exact SQL this store executes. Exposed for tests and for
// operators who want to EXPLAIN it.
func (s *Store) Query() string { return s.query }

// Config returns the effective configuration (defaults applied).
func (s *Store) Config() Config { return s.cfg }

// buildSearchQuery assembles the SELECT. Only validated, quoted identifiers are
// interpolated; the query vector and the limit are bound parameters ($1, $2).
//
// `<=>` is pgvector's cosine-distance operator, so similarity = 1 - distance.
func buildSearchQuery(cfg Config) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(quoteIdentifier(cfg.ContentColumn))
	b.WriteString(", COALESCE(")
	b.WriteString(quoteIdentifier(cfg.SourceColumn))
	b.WriteString(", '') AS source_url, 1 - (")
	b.WriteString(quoteIdentifier(cfg.EmbeddingColumn))
	b.WriteString(" <=> $1) AS similarity FROM ")
	b.WriteString(quoteIdentifier(cfg.Table))
	b.WriteString(" ORDER BY ")
	b.WriteString(quoteIdentifier(cfg.EmbeddingColumn))
	b.WriteString(" <=> $1 LIMIT $2")
	return b.String()
}

// Search implements stores.VectorStore.
func (s *Store) Search(ctx context.Context, vec []float32, topK int) ([]pipeline.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if topK <= 0 {
		return nil, nil
	}
	if topK > s.cfg.MaxTopK {
		topK = s.cfg.MaxTopK
	}
	if len(vec) == 0 {
		return nil, errors.New("pgvector: empty query vector")
	}

	rows, err := s.db.Query(ctx, s.query, FormatVector(vec), topK)
	if err != nil {
		return nil, fmt.Errorf("pgvector: query: %w", err)
	}
	defer rows.Close()

	out := make([]pipeline.Chunk, 0, topK)
	for rows.Next() {
		var c pipeline.Chunk
		if err := rows.Scan(&c.Content, &c.SourceURL, &c.Similarity); err != nil {
			return nil, fmt.Errorf("pgvector: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pgvector: rows: %w", err)
	}
	return out, nil
}

// FormatVector renders a vector in pgvector's text input format, e.g.
// "[0.1,0.2,0.3]". It is sent as a bound parameter, never concatenated into
// SQL, and contains only digits, '.', '-', 'e', ',' and brackets by
// construction.
func FormatVector(vec []float32) string {
	var b strings.Builder
	b.Grow(len(vec)*8 + 2)
	b.WriteByte('[')
	for i, v := range vec {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(v), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}
