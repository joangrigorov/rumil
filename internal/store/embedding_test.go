package store

import (
	"context"
	"strings"
	"testing"

	"github.com/joangrigorov/rumil/internal/testutil"
)

// declaredWidth is the width memories.embedding currently has.
func declaredWidth(t *testing.T, db *DB) int {
	t.Helper()

	width, err := db.declaredEmbeddingWidth(context.Background())
	if err != nil {
		t.Fatalf("reading the declared width of memories.embedding: %v", err)
	}
	return width
}

// hasEmbeddingIndex reports whether the index memory search uses is present.
func hasEmbeddingIndex(t *testing.T, db *DB) bool {
	t.Helper()

	var exists bool
	err := db.pool.QueryRow(context.Background(), `
		SELECT exists (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = current_schema() AND tablename = 'memories' AND indexname = $1)`,
		embeddingIndex).Scan(&exists)
	if err != nil {
		t.Fatalf("checking for %s: %v", embeddingIndex, err)
	}
	return exists
}

// The width memory runs at is the embedding provider's, so Open is what gives
// memories.embedding its width and the index over it: 001_init.sql declares the
// column bare because the width is not in SQL. A new width applies to an empty
// table, and is refused once rules are stored, because vectors of two widths
// cannot be compared with each other.
func TestEnsureEmbeddingWidth(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// The width outlives the rows and the tests in this package share a
	// schema, so the fixture state goes back before the next test opens it.
	t.Cleanup(func() {
		if _, err := db.pool.Exec(context.Background(), `TRUNCATE memories`); err != nil {
			t.Errorf("truncating memories: %v", err)
		}
		if err := db.EnsureEmbeddingWidth(context.Background(), testutil.EmbeddingDimensions); err != nil {
			t.Errorf("restoring the fixture width: %v", err)
		}
	})

	if got := declaredWidth(t, db); got != testutil.EmbeddingDimensions {
		t.Fatalf("the test database opened at %d dimensions, want %d", got, testutil.EmbeddingDimensions)
	}

	// Memory off leaves the column as it is, whatever width it has.
	if err := db.EnsureEmbeddingWidth(ctx, 0); err != nil {
		t.Fatalf("EnsureEmbeddingWidth(0) returned error: %v", err)
	}
	if got := declaredWidth(t, db); got != testutil.EmbeddingDimensions {
		t.Errorf("memory being off resized memories.embedding to %d, want it left alone", got)
	}

	// A narrower model, which is what a local embedding server serves.
	const localWidth = 768
	if err := db.EnsureEmbeddingWidth(ctx, localWidth); err != nil {
		t.Fatalf("EnsureEmbeddingWidth(%d) returned error: %v", localWidth, err)
	}
	if got := declaredWidth(t, db); got != localWidth {
		t.Errorf("memories.embedding is %d wide, want %d", got, localWidth)
	}
	if !hasEmbeddingIndex(t, db) {
		t.Error("idx_memories_embedding is missing after the resize, want it rebuilt at the new width")
	}

	// The same width again is a no-op rather than an error.
	if err := db.EnsureEmbeddingWidth(ctx, localWidth); err != nil {
		t.Fatalf("EnsureEmbeddingWidth(%d) a second time returned error: %v", localWidth, err)
	}

	// A rule stored at the local width makes a wider column impossible: the
	// stored vector is 768 wide and cannot be compared with a 1536-wide query.
	proposal := testProposal("money is decimal, never float", unitVector(localWidth))
	if _, inserted, err := db.ProposeMemory(ctx, proposal); err != nil {
		t.Fatalf("ProposeMemory returned error: %v", err)
	} else if !inserted {
		t.Fatal("ProposeMemory did not store the rule, want a fresh rule to be inserted")
	}

	err := db.EnsureEmbeddingWidth(ctx, testutil.EmbeddingDimensions)
	if err == nil {
		t.Fatal("EnsureEmbeddingWidth accepted a width the stored rules do not have")
	}
	for _, want := range []string{"768", "1536", "models.default_embedding"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}

	// The refusal leaves the column usable at the width it was at, so a run
	// that reads the configuration back is unaffected.
	if err := db.EnsureEmbeddingWidth(ctx, localWidth); err != nil {
		t.Fatalf("EnsureEmbeddingWidth(%d) after the refusal returned error: %v", localWidth, err)
	}
	if got := declaredWidth(t, db); got != localWidth {
		t.Errorf("memories.embedding is %d wide after the refusal, want %d", got, localWidth)
	}
}

// unitVector is a vector of the given width that has one value set, which is
// all a stored rule needs to occupy a column of that width.
func unitVector(width int) []float32 {
	v := make([]float32, width)
	v[0] = 1
	return v
}
