// Package pgtest provides a real PostgreSQL for integration tests.
//
// If TEST_DATABASE_URL is set it is used as-is (e.g. the docker-compose DB in
// CI). Otherwise an embedded PostgreSQL is downloaded (cached under
// ~/.embedded-postgres-go) and started on a free port for the test binary.
package pgtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wallet-transfer-service/internal/repository/postgres"
)

// Start returns a migrated pool and a teardown func. Call from TestMain.
func Start(ctx context.Context) (*pgxpool.Pool, func(), error) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	stopDB := func() {}

	if dsn == "" {
		port, err := freePort()
		if err != nil {
			return nil, nil, err
		}
		dir, err := os.MkdirTemp("", "wallet-pg-*")
		if err != nil {
			return nil, nil, err
		}
		db := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V16).
			Port(uint32(port)).
			Username("wallet").Password("wallet").Database("wallet").
			RuntimePath(filepath.Join(dir, "runtime")).
			DataPath(filepath.Join(dir, "data")).
			Logger(nil))
		if err := db.Start(); err != nil {
			return nil, nil, fmt.Errorf("start embedded postgres: %w", err)
		}
		stopDB = func() { _ = db.Stop(); _ = os.RemoveAll(dir) }
		dsn = fmt.Sprintf("postgres://wallet:wallet@localhost:%d/wallet?sslmode=disable", port)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		stopDB()
		return nil, nil, err
	}
	cfg.MaxConns = 32
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		stopDB()
		return nil, nil, err
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		pool.Close()
		stopDB()
		return nil, nil, err
	}
	return pool, func() { pool.Close(); stopDB() }, nil
}

// Reset empties all tables.
func Reset(t testing.TB, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`TRUNCATE ledger_entries, idempotency_records, transfers, wallets RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("reset db: %v", err)
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
