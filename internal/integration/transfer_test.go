package integration

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/service"
)

func TestTransferProcessed(t *testing.T) {
	e := newEnv(t)
	e.wallet("wallet_1", 1000)
	e.wallet("wallet_2", 0)

	res, err := e.svc.CreateTransfer(e.ctx, req("abc123", "wallet_1", "wallet_2", 100))
	if err != nil {
		t.Fatal(err)
	}
	if res.Replayed || res.Transfer.Status != domain.TransferProcessed || res.Transfer.IdempotencyKey != "abc123" {
		t.Fatalf("unexpected result %+v", res)
	}
	if b := e.balance("wallet_1"); b != 900 {
		t.Fatalf("wallet_1 balance = %d, want 900", b)
	}
	if b := e.balance("wallet_2"); b != 100 {
		t.Fatalf("wallet_2 balance = %d, want 100", b)
	}
	entries, err := e.svc.ListLedgerEntries(e.ctx, "wallet_1")
	if err != nil || len(entries) != 1 || entries[0].Type != domain.EntryDebit || entries[0].Amount != 100 {
		t.Fatalf("wallet_1 ledger = %+v, %v", entries, err)
	}
	got, err := e.svc.GetTransfer(e.ctx, res.Transfer.ID)
	if err != nil || got.Status != domain.TransferProcessed {
		t.Fatalf("GetTransfer = %+v, %v", got, err)
	}
	e.assertLedgerInvariants()
}

func TestIdempotentReplayReturnsOriginalResult(t *testing.T) {
	e := newEnv(t)
	e.wallet("a", 1000)
	e.wallet("b", 0)

	first, err := e.svc.CreateTransfer(e.ctx, req("k1", "a", "b", 100))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		again, err := e.svc.CreateTransfer(e.ctx, req("k1", "a", "b", 100))
		if err != nil {
			t.Fatal(err)
		}
		if !again.Replayed || again.Transfer.ID != first.Transfer.ID || again.Transfer.Status != first.Transfer.Status {
			t.Fatalf("replay %d = %+v, want original %+v", i, again, first)
		}
	}
	if b := e.balance("a"); b != 900 {
		t.Fatalf("balance = %d, want 900 (debited once)", b)
	}
	if n := e.count(`SELECT count(*) FROM transfers`); n != 1 {
		t.Fatalf("%d transfers, want 1", n)
	}
	e.assertLedgerInvariants()
}

func TestIdempotencyKeyReusedWithDifferentPayload(t *testing.T) {
	e := newEnv(t)
	e.wallet("a", 1000)
	e.wallet("b", 0)
	e.wallet("c", 0)

	if _, err := e.svc.CreateTransfer(e.ctx, req("k1", "a", "b", 100)); err != nil {
		t.Fatal(err)
	}
	for _, r := range []domain.TransferRequest{req("k1", "a", "b", 101), req("k1", "a", "c", 100)} {
		if _, err := e.svc.CreateTransfer(e.ctx, r); !errors.Is(err, domain.ErrIdempotencyKeyReused) {
			t.Fatalf("%+v: want ErrIdempotencyKeyReused, got %v", r, err)
		}
	}
	if b := e.balance("a"); b != 900 {
		t.Fatalf("balance = %d, want 900", b)
	}
	if n := e.count(`SELECT count(*) FROM transfers`); n != 1 {
		t.Fatalf("%d transfers, want 1 (rejected requests must not persist)", n)
	}
}

func TestInsufficientFundsFailsAndReplaysFailure(t *testing.T) {
	e := newEnv(t)
	e.wallet("a", 50)
	e.wallet("b", 0)

	res, err := e.svc.CreateTransfer(e.ctx, req("k1", "a", "b", 100))
	if err != nil {
		t.Fatal(err)
	}
	if res.Transfer.Status != domain.TransferFailed || res.Transfer.FailureReason != domain.FailureInsufficientFunds {
		t.Fatalf("got %+v, want FAILED/INSUFFICIENT_FUNDS", res.Transfer)
	}
	if e.balance("a") != 50 || e.balance("b") != 0 {
		t.Fatal("balances changed on a failed transfer")
	}

	// Fund the wallet out-of-band; the original (failed) result must still
	// be replayed for the same key.
	if _, err := pool.Exec(e.ctx, `UPDATE wallets SET balance = 500, opening_balance = 500 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.CreateTransfer(e.ctx, req("k1", "a", "b", 100))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.Transfer.ID != res.Transfer.ID || again.Transfer.Status != domain.TransferFailed {
		t.Fatalf("replay = %+v, want original FAILED transfer", again)
	}
	if e.balance("a") != 500 {
		t.Fatal("replay of a failed transfer moved money")
	}
	e.assertLedgerInvariants()
}

func TestWalletNotFoundDoesNotConsumeKey(t *testing.T) {
	e := newEnv(t)
	e.wallet("a", 100)

	if _, err := e.svc.CreateTransfer(e.ctx, req("k1", "a", "missing", 10)); !errors.Is(err, domain.ErrWalletNotFound) {
		t.Fatalf("want ErrWalletNotFound, got %v", err)
	}
	if n := e.count(`SELECT count(*) FROM transfers`); n != 0 {
		t.Fatalf("%d transfers persisted for an invalid request", n)
	}
	e.wallet("missing", 0)
	res, err := e.svc.CreateTransfer(e.ctx, req("k1", "a", "missing", 10))
	if err != nil || res.Replayed || res.Transfer.Status != domain.TransferProcessed {
		t.Fatalf("retry after fixing request = %+v, %v", res, err)
	}
}

func TestTransfersWithoutKeyAreNotDeduplicated(t *testing.T) {
	e := newEnv(t)
	e.wallet("a", 100)
	e.wallet("b", 0)
	for i := 0; i < 2; i++ {
		if _, err := e.svc.CreateTransfer(e.ctx, req("", "a", "b", 10)); err != nil {
			t.Fatal(err)
		}
	}
	if b := e.balance("a"); b != 80 {
		t.Fatalf("balance = %d, want 80", b)
	}
	e.assertLedgerInvariants()
}

// A crash after Tx 1 leaves a PENDING transfer; a retry with the same key must
// finish it exactly once.
func TestRetryResumesPendingTransfer(t *testing.T) {
	e := newEnv(t)
	e.wallet("a", 100)
	e.wallet("b", 0)

	r := req("k1", "a", "b", 30)
	pending, _ := domain.NewTransfer(uuid.NewString(), r)
	err := e.store.WithTx(e.ctx, func(tx service.Tx) error {
		if err := tx.InsertTransfer(e.ctx, &pending); err != nil {
			return err
		}
		_, err := tx.InsertIdempotencyRecord(e.ctx, service.IdempotencyRecord{
			Key: r.IdempotencyKey, RequestHash: r.Fingerprint(), TransferID: pending.ID,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.balance("a") != 100 {
		t.Fatal("PENDING transfer must not move money")
	}

	res, err := e.svc.CreateTransfer(e.ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if res.Transfer.ID != pending.ID || res.Transfer.Status != domain.TransferProcessed || !res.Replayed {
		t.Fatalf("got %+v, want resumed transfer %s PROCESSED", res, pending.ID)
	}
	if _, err := e.svc.CreateTransfer(e.ctx, r); err != nil {
		t.Fatal(err)
	}
	if e.balance("a") != 70 || e.balance("b") != 30 {
		t.Fatalf("balances a=%d b=%d, want 70/30", e.balance("a"), e.balance("b"))
	}
	e.assertLedgerInvariants()
}

// The repository refuses to move a transfer out of a terminal state even if
// the caller skips the domain check.
func TestRepositoryGuardsTerminalState(t *testing.T) {
	e := newEnv(t)
	e.wallet("a", 100)
	e.wallet("b", 0)
	res, err := e.svc.CreateTransfer(e.ctx, req("k1", "a", "b", 10))
	if err != nil {
		t.Fatal(err)
	}
	forged := res.Transfer
	forged.Status = domain.TransferFailed
	forged.FailureReason = domain.FailureInsufficientFunds
	err = e.store.WithTx(e.ctx, func(tx service.Tx) error {
		return tx.CompletePendingTransfer(e.ctx, &forged)
	})
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
	got, _ := e.svc.GetTransfer(e.ctx, res.Transfer.ID)
	if got.Status != domain.TransferProcessed {
		t.Fatalf("status changed to %s", got.Status)
	}
}

// ---- concurrency ----

func TestConcurrentDuplicatesExecuteOnce(t *testing.T) {
	e := newEnv(t)
	e.wallet("a", 1000)
	e.wallet("b", 0)

	const n = 30
	results := make([]service.TransferResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = e.svc.CreateTransfer(e.ctx, req("dup", "a", "b", 100))
		}(i)
	}
	close(start)
	wg.Wait()

	replayed := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if results[i].Transfer.ID != results[0].Transfer.ID || results[i].Transfer.Status != domain.TransferProcessed {
			t.Fatalf("request %d got %+v, want transfer %s PROCESSED", i, results[i].Transfer, results[0].Transfer.ID)
		}
		if results[i].Replayed {
			replayed++
		}
	}
	if replayed != n-1 {
		t.Fatalf("%d replays, want %d", replayed, n-1)
	}
	if b := e.balance("a"); b != 900 {
		t.Fatalf("balance = %d, want 900", b)
	}
	if c := e.count(`SELECT count(*) FROM transfers`); c != 1 {
		t.Fatalf("%d transfers, want 1", c)
	}
	e.assertLedgerInvariants()
}

func TestConcurrentDebitsNoDoubleSpend(t *testing.T) {
	e := newEnv(t)
	e.wallet("src", 100)
	const n = 30 // 30 x 10 requested, only 100 available
	for i := 0; i < n; i++ {
		e.wallet(fmt.Sprintf("dst%02d", i), 0)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	processed, failed := 0, 0
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := e.svc.CreateTransfer(e.ctx, req(fmt.Sprintf("k%d", i), "src", fmt.Sprintf("dst%02d", i), 10))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("request %d: %v", i, err)
			case res.Transfer.Status == domain.TransferProcessed:
				processed++
			case res.Transfer.Status == domain.TransferFailed:
				failed++
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if processed != 10 || failed != n-10 {
		t.Fatalf("processed=%d failed=%d, want 10/%d", processed, failed, n-10)
	}
	if b := e.balance("src"); b != 0 {
		t.Fatalf("src balance = %d, want 0", b)
	}
	e.assertLedgerInvariants()
}

// Opposite-direction transfers between the same wallets would deadlock with
// unordered locking; ordered locking must let all of them complete.
func TestConcurrentOppositeTransfersNoDeadlock(t *testing.T) {
	e := newEnv(t)
	e.wallet("x", 10_000)
	e.wallet("y", 10_000)

	const n = 40
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			from, to := "x", "y"
			if i%2 == 1 {
				from, to = "y", "x"
			}
			res, err := e.svc.CreateTransfer(e.ctx, req(fmt.Sprintf("k%d", i), from, to, int64(i+1)))
			if err != nil {
				t.Errorf("request %d: %v", i, err)
			} else if res.Transfer.Status != domain.TransferProcessed {
				t.Errorf("request %d: status %s", i, res.Transfer.Status)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if total := e.balance("x") + e.balance("y"); total != 20_000 {
		t.Fatalf("money not conserved: total=%d", total)
	}
	if c := e.count(`SELECT count(*) FROM ledger_entries`); c != 2*n {
		t.Fatalf("%d ledger entries, want %d", c, 2*n)
	}
	e.assertLedgerInvariants()
}
