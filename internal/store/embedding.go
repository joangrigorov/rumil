package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// embeddingIndex is the index memory search runs against. It is created here
// rather than in 001_init.sql because pgvector builds an HNSW index only on a
// column of a fixed width, and the width is not in the migration.
const embeddingIndex = "idx_memories_embedding"

const indexEmbeddingSQL = `CREATE INDEX IF NOT EXISTS ` + embeddingIndex +
	` ON memories USING hnsw (embedding vector_cosine_ops)`

// EnsureEmbeddingWidth sizes memories.embedding to width and builds the index
// that searches it. 001_init.sql declares the column without a width because
// the width is the embedding provider's, which SQL cannot read from the
// configuration; this is what puts it there, once per start.
//
// The width is also what makes a stored vector mean anything, so a table that
// already holds rules of another width is refused rather than left holding two
// sets of vectors that cannot be compared with each other.
func (d *DB) EnsureEmbeddingWidth(ctx context.Context, width int) error {
	if width <= 0 {
		return nil
	}

	declared, err := d.declaredEmbeddingWidth(ctx)
	if err != nil {
		return err
	}
	if declared == width {
		return d.indexEmbedding(ctx)
	}

	// The column is bare or typed to another width. What the stored vectors
	// are decides whether it can be resized: an empty table takes any width,
	// and vectors of the right width survive the change.
	stored, err := d.storedEmbeddingWidth(ctx)
	if err != nil {
		return err
	}
	if stored != 0 && stored != width {
		return fmt.Errorf("memories.embedding holds %d-dimension vectors and the embedding provider is %d: stored rules cannot be compared with vectors of another width, so point models.default_embedding at a %d-dimension model, or empty the table and record the rules again",
			stored, width, stored)
	}
	return d.resizeEmbedding(ctx, width)
}

// declaredEmbeddingWidth is the width memories.embedding was declared with, or
// zero for the bare vector the migration declares.
func (d *DB) declaredEmbeddingWidth(ctx context.Context) (int, error) {
	var typeName string
	err := d.pool.QueryRow(ctx, `
		SELECT format_type(atttypid, atttypmod) FROM pg_attribute
		WHERE attrelid = to_regclass('memories') AND attname = 'embedding' AND NOT attisdropped`).
		Scan(&typeName)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("the memories table has no embedding column: apply the schema first, which database.auto_migrate does on startup")
	}
	if err != nil {
		return 0, fmt.Errorf("reading the type of memories.embedding: %w", err)
	}
	return vectorWidth(typeName), nil
}

// storedEmbeddingWidth is the width of a vector already in the table, or zero
// when the table holds no rules.
func (d *DB) storedEmbeddingWidth(ctx context.Context) (int, error) {
	var width int
	err := d.pool.QueryRow(ctx,
		`SELECT vector_dims(embedding) FROM memories WHERE embedding IS NOT NULL LIMIT 1`).Scan(&width)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading the width of a stored embedding: %w", err)
	}
	return width, nil
}

// resizeEmbedding types the column to width and rebuilds the index over it.
// The index goes first, because it cannot outlive the width it was built for.
// The width reached the configuration as a number and was validated there, so
// the statement interpolates digits and nothing else.
func (d *DB) resizeEmbedding(ctx context.Context, width int) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sizing memories.embedding: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DROP INDEX IF EXISTS `+embeddingIndex); err != nil {
		return fmt.Errorf("dropping %s to resize memories.embedding: %w", embeddingIndex, err)
	}
	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`ALTER TABLE memories ALTER COLUMN embedding TYPE vector(%d)`, width)); err != nil {
		return fmt.Errorf("resizing memories.embedding to %d dimensions: %w", width, err)
	}
	if _, err := tx.Exec(ctx, indexEmbeddingSQL); err != nil {
		return fmt.Errorf("building %s: %w", embeddingIndex, err)
	}
	return tx.Commit(ctx)
}

// indexEmbedding makes sure the index over memories.embedding exists, which is
// all a column of the right width still needs.
func (d *DB) indexEmbedding(ctx context.Context) error {
	if _, err := d.pool.Exec(ctx, indexEmbeddingSQL); err != nil {
		return fmt.Errorf("building %s: %w", embeddingIndex, err)
	}
	return nil
}

// vectorWidth reads the width out of a type name such as "vector(768)", and
// reports zero for a vector declared without one.
func vectorWidth(typeName string) int {
	open := strings.IndexByte(typeName, '(')
	if open < 0 || !strings.HasSuffix(typeName, ")") {
		return 0
	}
	width, err := strconv.Atoi(typeName[open+1 : len(typeName)-1])
	if err != nil {
		return 0
	}
	return width
}
