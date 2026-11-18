// Package integration exercises the service, repository and HTTP layers
// against a real PostgreSQL, including concurrent scenarios.
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/repository/postgres"
	"github.com/wallet-transfer-service/internal/service"
	"github.com/wallet-transfer-service/internal/testutil/pgtest"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	p, teardown, err := pgtest.Start(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration tests need PostgreSQL:", err)
		os.Exit(1)
	}
	pool = p
	code := m.Run()
	teardown()
	os.Exit(code)
}

type env struct {
	t     *testing.T
	ctx   context.Context
	store *postgres.Store
	svc   *service.TransferService
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pgtest.Reset(t, pool)
	store := postgres.New(pool)
	return &env{t: t, ctx: context.Background(), store: store, svc: service.New(store, nil)}
}

func (e *env) wallet(id string, balance int64) {
	e.t.Helper()
	if _, err := e.svc.CreateWallet(e.ctx, id, balance); err != nil {
		e.t.Fatalf("create wallet %s: %v", id, err)
	}
}

func (e *env) balance(id string) int64 {
	e.t.Helper()
	w, err := e.svc.GetWallet(e.ctx, id)
	if err != nil {
		e.t.Fatalf("get wallet %s: %v", id, err)
	}
	return w.Balance
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := pool.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// assertLedgerInvariants checks the double-entry rules over the whole DB.
func (e *env) assertLedgerInvariants() {
	e.t.Helper()
	// Each check is a query that returns the number of violating rows; every
	// one must be zero. Keeping them separate makes a failure self-explaining.
	checks := []struct{ rule, sql string }{
		{
			"every PROCESSED transfer has exactly two ledger entries",
			`SELECT count(*) FROM transfers t
			 WHERE t.status = 'PROCESSED'
			   AND (SELECT count(*) FROM ledger_entries l WHERE l.transfer_id = t.id) <> 2`,
		},
		{
			"every PROCESSED transfer has one DEBIT from the source and one CREDIT to the destination",
			`SELECT count(*) FROM transfers t
			 WHERE t.status = 'PROCESSED' AND NOT (
			   EXISTS (SELECT 1 FROM ledger_entries l WHERE l.transfer_id = t.id
			           AND l.type = 'DEBIT'  AND l.wallet_id = t.from_wallet_id)
			   AND EXISTS (SELECT 1 FROM ledger_entries l WHERE l.transfer_id = t.id
			           AND l.type = 'CREDIT' AND l.wallet_id = t.to_wallet_id))`,
		},
		{
			"debit amount = credit amount = transfer amount",
			`SELECT count(*) FROM ledger_entries l JOIN transfers t ON t.id = l.transfer_id
			 WHERE l.amount <> t.amount`,
		},
		{
			"FAILED and PENDING transfers have no ledger entries",
			`SELECT count(*) FROM ledger_entries l JOIN transfers t ON t.id = l.transfer_id
			 WHERE t.status <> 'PROCESSED'`,
		},
		{
			"ledger balances globally (sum of debits = sum of credits)",
			`SELECT count(*) FROM (
			   SELECT SUM(CASE type WHEN 'DEBIT' THEN amount ELSE -amount END) AS net
			   FROM ledger_entries) s
			 WHERE COALESCE(s.net, 0) <> 0`,
		},
		{
			"wallet balance = opening balance + credits - debits",
			`SELECT count(*) FROM wallets w
			 WHERE w.balance <> w.opening_balance
			   + COALESCE((SELECT SUM(amount) FROM ledger_entries WHERE wallet_id = w.id AND type = 'CREDIT'), 0)
			   - COALESCE((SELECT SUM(amount) FROM ledger_entries WHERE wallet_id = w.id AND type = 'DEBIT'), 0)`,
		},
	}
	for _, c := range checks {
		if n := e.count(c.sql); n != 0 {
			e.t.Errorf("ledger invariant violated (%d rows): %s", n, c.rule)
		}
	}
}

func req(key, from, to string, amount int64) domain.TransferRequest {
	return domain.TransferRequest{IdempotencyKey: key, FromWalletID: from, ToWalletID: to, Amount: amount}
}
