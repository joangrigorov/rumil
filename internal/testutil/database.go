package testutil

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDatabaseURL returns TEST_DATABASE_URL pointed at a schema named after the
// caller, so test packages that `go test ./...` runs in parallel never share a
// table. It creates the schema, and skips the test when the variable is unset.
//
// The schema name is caller-supplied and never user input.
func TestDatabaseURL(t *testing.T, schema string) string {
	t.Helper()

	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL is not set, skipping the database tests")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, base)
	if err != nil {
		t.Fatalf("connecting to create the test schema: %v", err)
	}
	defer pool.Close()

	// The public schema stays on the path so the vector type and pgcrypto
	// resolve when they are installed database-wide rather than per schema.
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("creating the %s schema: %v", schema, err)
	}

	options := url.QueryEscape(fmt.Sprintf("-csearch_path=%s,public", schema))
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	return base + separator + "options=" + options
}
