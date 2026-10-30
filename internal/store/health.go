package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// undefinedTable is Postgres's SQLSTATE for a table that does not exist. It is
// how a database the schema has never been applied to answers the version read,
// which is a version of zero rather than a failure of the read.
const undefinedTable = "42P01"

// MigrationVersion is the highest schema version applied to the database. A
// database without the goose table reports zero, so a caller can tell "nothing
// applied yet" apart from "the read failed".
//
// The read is a SELECT of the bookkeeping table rather than goose's own
// GetDBVersion, which creates that table when it is missing; a check must not
// write.
func (d *DB) MigrationVersion(ctx context.Context) (int64, error) {
	var version int64
	err := d.pool.QueryRow(ctx,
		`SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&version)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
			return 0, nil
		}
		return 0, fmt.Errorf("reading the applied schema version: %w", err)
	}
	return version, nil
}

// LatestMigrationVersion is the highest version embedded in this binary. A
// binary older than the schema it opens is normal during a rollback; a schema
// behind the binary is what doctor reports.
func LatestMigrationVersion() (int64, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return 0, fmt.Errorf("reading the embedded migrations: %w", err)
	}

	var latest int64
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			return 0, fmt.Errorf("the embedded migration %q does not start with a version", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("the embedded migration %q has no numeric version: %w", name, err)
		}
		if version > latest {
			latest = version
		}
	}
	if latest == 0 {
		return 0, errors.New("the binary embeds no migrations")
	}
	return latest, nil
}

// HasExtension reports whether the database has an extension installed. The
// vector extension is created by the first migration, so a database whose
// schema was applied by hand without it answers false.
func (d *DB) HasExtension(ctx context.Context, name string) (bool, error) {
	var installed bool
	if err := d.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)`, name).Scan(&installed); err != nil {
		return false, fmt.Errorf("reading whether the %s extension is installed: %w", name, err)
	}
	return installed, nil
}
