package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/repository/postgres"
	"github.com/wallet-transfer-service/internal/service"
)

var errInjected = errors.New("injected database failure")

// faultyStore is the real Postgres store, but its transactions fail at a
// chosen step of the process phase. Writes made earlier in the same
// transaction really happen and must be rolled back by Postgres.
type faultyStore struct {
	*postgres.Store
	failAt string
}

func (s *faultyStore) WithTx(ctx context.Context, fn func(service.Tx) error) error {
	return s.Store.WithTx(ctx, func(tx service.Tx) error {
		return fn(&faultyTx{Tx: tx, failAt: s.failAt})
	})
}

type faultyTx struct {
	service.Tx
	failAt        string
	walletUpdates int
}

func (t *faultyTx) UpdateWalletBalance(ctx context.Context, w *domain.Wallet) error {
	if err := t.Tx.UpdateWalletBalance(ctx, w); err != nil {
		return err
	}
	t.walletUpdates++
	if t.failAt == "after-debit" && t.walletUpdates == 1 {
		return errInjected
	}
	return nil
}

func (t *faultyTx) InsertLedgerEntries(ctx context.Context, entries []domain.LedgerEntry) error {
	if t.failAt == "mid-ledger" {
		// Persist the DEBIT, then fail before the CREDIT.
		if err := t.Tx.InsertLedgerEntries(ctx, entries[:1]); err != nil {
			return err
		}
		return errInjected
	}
	if t.failAt == "connection-drop" {
		// Persist the DEBIT, then really kill this transaction's server
		// process from another connection. The CREDIT insert then fails on a
		// dead connection and Postgres discards the open transaction.
		if err := t.Tx.InsertLedgerEntries(ctx, entries[:1]); err != nil {
			return err
		}
		var killed int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM (
			  SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			  WHERE datname = current_database()
			    AND state = 'idle in transaction'
			    AND pid <> pg_backend_pid()) k`).Scan(&killed); err != nil || killed != 1 {
			return fmt.Errorf("test setup: terminated %d backends: %v", killed, err)
		}
		return t.Tx.InsertLedgerEntries(ctx, entries[1:])
	}
	return t.Tx.InsertLedgerEntries(ctx, entries)
}

func (t *faultyTx) CompletePendingTransfer(ctx context.Context, tr *domain.Transfer) error {
	if t.failAt == "status-update" {
		return errInjected // balances and both ledger rows already written
	}
	return t.Tx.CompletePendingTransfer(ctx, tr)
}

// A failure at any step of processing rolls back balances, ledger entries
// and status together; a retry with the same key then completes it once.
func TestFailureDuringProcessingRollsBackAndRetryRecovers(t *testing.T) {
	for _, failAt := range []string{"after-debit", "mid-ledger", "status-update", "connection-drop"} {
		t.Run(failAt, func(t *testing.T) {
			e := newEnv(t)
			e.wallet("a", 100)
			e.wallet("b", 0)
			r := req("k1", "a", "b", 40)

			broken := service.New(&faultyStore{Store: e.store, failAt: failAt}, nil)
			_, err := broken.CreateTransfer(e.ctx, r)
			switch {
			case err == nil:
				t.Fatal("want an error, got success")
			case failAt != "connection-drop" && !errors.Is(err, errInjected):
				t.Fatalf("want injected error, got %v", err)
			case strings.HasPrefix(err.Error(), "test setup"):
				t.Fatal(err)
			}

			// Full rollback of Tx 2; Tx 1 (the PENDING intent) survived.
			if e.balance("a") != 100 || e.balance("b") != 0 {
				t.Fatalf("balances changed: a=%d b=%d", e.balance("a"), e.balance("b"))
			}
			if n := e.count(`SELECT count(*) FROM ledger_entries`); n != 0 {
				t.Fatalf("%d ledger entries survived the rollback", n)
			}
			if n := e.count(`SELECT count(*) FROM transfers WHERE status = 'PENDING'`); n != 1 {
				t.Fatalf("%d PENDING transfers, want 1", n)
			}
			e.assertLedgerInvariants()

			// The client retries with the same key against a healthy service.
			res, err := e.svc.CreateTransfer(e.ctx, r)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Replayed || res.Transfer.Status != domain.TransferProcessed {
				t.Fatalf("retry = %+v, want replayed PROCESSED", res)
			}
			if e.balance("a") != 60 || e.balance("b") != 40 {
				t.Fatalf("after retry a=%d b=%d, want 60/40", e.balance("a"), e.balance("b"))
			}
			if n := e.count(`SELECT count(*) FROM transfers`); n != 1 {
				t.Fatalf("%d transfers, want 1", n)
			}
			e.assertLedgerInvariants()
		})
	}
}

// A client that disconnects (cancelled request context) must not strand the
// transfer in PENDING: once validated, the work runs to completion. A retry
// after the timeout then replays the original result.
func TestClientDisconnectStillCompletesTransfer(t *testing.T) {
	for _, key := range []string{"k1", ""} { // keyless requests can't be resumed, so this matters most for them
		t.Run("key="+key, func(t *testing.T) {
			e := newEnv(t)
			e.wallet("a", 100)
			e.wallet("b", 0)
			r := req(key, "a", "b", 25)

			ctx, cancel := context.WithCancel(e.ctx)
			cancel() // client already gone
			res, err := e.svc.CreateTransfer(ctx, r)
			if err != nil || res.Transfer.Status != domain.TransferProcessed {
				t.Fatalf("got %+v, %v; want PROCESSED despite disconnect", res, err)
			}
			if n := e.count(`SELECT count(*) FROM transfers WHERE status = 'PENDING'`); n != 0 {
				t.Fatalf("%d transfers stranded in PENDING", n)
			}

			if key != "" {
				again, err := e.svc.CreateTransfer(e.ctx, r)
				if err != nil || !again.Replayed || again.Transfer.ID != res.Transfer.ID {
					t.Fatalf("retry = %+v, %v; want replay of %s", again, err, res.Transfer.ID)
				}
			}
			if e.balance("a") != 75 {
				t.Fatalf("balance = %d, want 75 (debited once)", e.balance("a"))
			}
			e.assertLedgerInvariants()
		})
	}
}

// Invalid requests are rejected before anything is persisted.
func TestInvalidRequestsPersistNothing(t *testing.T) {
	e := newEnv(t)
	e.wallet("a", 100)
	e.wallet("b", 0)

	cases := []struct {
		name string
		req  domain.TransferRequest
		want error
	}{
		{"same wallet", req("k1", "a", "a", 10), domain.ErrValidation},
		{"zero amount", req("k2", "a", "b", 0), domain.ErrValidation},
		{"negative amount", req("k3", "a", "b", -10), domain.ErrValidation},
		{"nonexistent source", req("k4", "nope", "b", 10), domain.ErrWalletNotFound},
		{"nonexistent destination", req("k5", "a", "nope", 10), domain.ErrWalletNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := e.svc.CreateTransfer(e.ctx, c.req); !errors.Is(err, c.want) {
				t.Fatalf("want %v, got %v", c.want, err)
			}
		})
	}
	if n := e.count(`SELECT count(*) FROM transfers`) + e.count(`SELECT count(*) FROM idempotency_records`); n != 0 {
		t.Fatalf("%d rows persisted for invalid requests", n)
	}
	if e.balance("a") != 100 || e.balance("b") != 0 {
		t.Fatal("balances changed")
	}
}

// The schema rejects corrupt states even if application code were bypassed.
func TestDatabaseConstraintsBackstop(t *testing.T) {
	e := newEnv(t)
	e.wallet("a", 100)
	e.wallet("b", 0)
	res, err := e.svc.CreateTransfer(e.ctx, req("k1", "a", "b", 10))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Transfer.ID

	cases := map[string]string{
		"negative balance":   `UPDATE wallets SET balance = -1 WHERE id = 'a'`,
		"second DEBIT":       `INSERT INTO ledger_entries (wallet_id, transfer_id, type, amount) VALUES ('a', '` + id + `', 'DEBIT', 10)`,
		"zero ledger amount": `INSERT INTO ledger_entries (wallet_id, transfer_id, type, amount) VALUES ('a', '` + id + `', 'DEBIT', 0)`,
		"unknown status":     `UPDATE transfers SET status = 'REVERSED' WHERE id = '` + id + `'`,
		"FAILED w/o reason":  `UPDATE transfers SET status = 'FAILED' WHERE id = '` + id + `'`,
		"self transfer":      `UPDATE transfers SET to_wallet_id = from_wallet_id WHERE id = '` + id + `'`,
	}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := pool.Exec(e.ctx, sql); err == nil {
				t.Fatalf("schema accepted: %s", sql)
			}
		})
	}
	e.assertLedgerInvariants()
}
