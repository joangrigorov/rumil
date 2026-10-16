package testutil

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

// postgresImage is the database every database-backed test runs against: the
// same image rumil.example.yaml and docker-compose.yml deploy, so pgvector and
// the extension versions a query here depends on are the ones that ship.
const postgresImage = "pgvector/pgvector:pg16"

// RequireDockerEnv turns "this machine has no usable Docker" from a skip into a
// failure. CI sets it, so a runner that cannot start containers fails the build
// rather than quietly running the unit tests alone.
const RequireDockerEnv = "RUMIL_REQUIRE_DOCKER"

// startupTimeout bounds one container's start, so a wedged Docker daemon fails
// the test instead of hanging the run.
const startupTimeout = 2 * time.Minute

// testDatabase is the one container a test process runs against. `go test ./...`
// runs one process per package, so each package starts its own and no package
// can see another's tables; the packages run in parallel and pay a few seconds
// each for it.
var testDatabase struct {
	once sync.Once
	base string
	err  error
}

// TestDatabaseURL returns a DSN for a database this test process owns: a
// pgvector container started on first use, aimed at a schema named after the
// caller, so the tests of one package never share a table.
//
// The container is left for testcontainers' reaper, which removes it when the
// process exits. There is no point in the test lifecycle after a package's last
// test to terminate it at, and the reaper covers a panic as well as a pass.
//
// The schema name is caller-supplied and never user input.
func TestDatabaseURL(t *testing.T, schema string) string {
	t.Helper()

	base, err := testDatabaseDSN()
	if err != nil {
		if os.Getenv(RequireDockerEnv) != "" {
			t.Fatalf("%s is set, and the test database could not be started: %v", RequireDockerEnv, err)
		}
		t.Skipf("the database tests need a pgvector container, which could not be started: %v; set %s=1 to fail instead",
			err, RequireDockerEnv)
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

// testDatabaseDSN starts the container once per process and hands back its DSN.
func testDatabaseDSN() (string, error) {
	testDatabase.once.Do(func() {
		testDatabase.base, testDatabase.err = startPostgres()
	})
	return testDatabase.base, testDatabase.err
}

func startPostgres() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()

	container, err := postgres.Run(ctx, postgresImage,
		postgres.WithDatabase("rumil"),
		postgres.WithUsername("rumil"),
		postgres.WithPassword("rumil"),
		postgres.BasicWaitStrategies(),
		testcontainers.WithLabels(map[string]string{"com.rumil.test": "database"}),
	)
	if err != nil {
		return "", fmt.Errorf("starting %s: %w", postgresImage, err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", fmt.Errorf("reading the DSN of the test database: %w", err)
	}
	return dsn, nil
}
