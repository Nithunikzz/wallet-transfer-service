package domain

import (
	"errors"
	"testing"
)

func validReq() TransferRequest {
	return TransferRequest{IdempotencyKey: "k", FromWalletID: "a", ToWalletID: "b", Amount: 100}
}

func TestTransferRequestValidate(t *testing.T) {
	long := string(make([]byte, 256))
	cases := map[string]func(*TransferRequest){
		"zero amount":     func(r *TransferRequest) { r.Amount = 0 },
		"negative amount": func(r *TransferRequest) { r.Amount = -1 },
		"same wallet":     func(r *TransferRequest) { r.ToWalletID = r.FromWalletID },
		"missing from":    func(r *TransferRequest) { r.FromWalletID = "" },
		"missing to":      func(r *TransferRequest) { r.ToWalletID = "" },
		"key too long":    func(r *TransferRequest) { r.IdempotencyKey = long },
		"wallet too long": func(r *TransferRequest) { r.ToWalletID = long },
		"wallet bad char": func(r *TransferRequest) { r.FromWalletID = "a b" },
		"wallet slash":    func(r *TransferRequest) { r.ToWalletID = "../x" },
		"key with space":  func(r *TransferRequest) { r.IdempotencyKey = "a b" },
		"key non-ascii":   func(r *TransferRequest) { r.IdempotencyKey = "clé" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := validReq()
			mutate(&r)
			if err := r.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
		})
	}
	if err := validReq().Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	noKey := validReq()
	noKey.IdempotencyKey = ""
	if err := noKey.Validate(); err != nil {
		t.Fatalf("idempotency key must be optional: %v", err)
	}
}

func TestFingerprint(t *testing.T) {
	a := validReq()
	b := validReq()
	b.IdempotencyKey = "other" // key is not part of the payload identity
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("fingerprint must not depend on the idempotency key")
	}
	b.Amount = 101
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("fingerprint must change with the amount")
	}
	// Field boundaries must be unambiguous.
	x := TransferRequest{FromWalletID: "ab", ToWalletID: "c", Amount: 1}
	y := TransferRequest{FromWalletID: "a", ToWalletID: "bc", Amount: 1}
	if x.Fingerprint() == y.Fingerprint() {
		t.Fatal("fingerprint collision across field boundaries")
	}
}

func TestTransferStateMachine(t *testing.T) {
	tr, err := NewTransfer("t1", validReq())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != TransferPending {
		t.Fatalf("new transfer must be PENDING, got %s", tr.Status)
	}
	if err := tr.MarkProcessed(); err != nil {
		t.Fatal(err)
	}
	if err := tr.MarkProcessed(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("PROCESSED -> PROCESSED must fail, got %v", err)
	}
	if err := tr.MarkFailed(FailureInsufficientFunds); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("PROCESSED -> FAILED must fail, got %v", err)
	}

	tr2, _ := NewTransfer("t2", validReq())
	if err := tr2.MarkFailed(FailureNone); !errors.Is(err, ErrValidation) {
		t.Fatalf("FAILED requires a reason, got %v", err)
	}
	if err := tr2.MarkFailed(FailureInsufficientFunds); err != nil {
		t.Fatal(err)
	}
	if err := tr2.MarkProcessed(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("FAILED -> PROCESSED must fail, got %v", err)
	}
	if !tr2.Status.IsTerminal() || TransferPending.IsTerminal() {
		t.Fatal("IsTerminal mismatch")
	}
}

func TestLedgerEntriesBalance(t *testing.T) {
	tr, _ := NewTransfer("t1", validReq())
	e := tr.LedgerEntries()
	if e[0].Type != EntryDebit || e[0].WalletID != "a" || e[1].Type != EntryCredit || e[1].WalletID != "b" {
		t.Fatalf("unexpected entries %+v", e)
	}
	if e[0].Amount != e[1].Amount || e[0].Amount != 100 {
		t.Fatal("entries must carry equal amounts")
	}
}

func TestWalletDebitCredit(t *testing.T) {
	w, err := NewWallet("w", 50)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Debit(51); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("want ErrInsufficientFunds, got %v", err)
	}
	if err := w.Debit(50); err != nil || w.Balance != 0 {
		t.Fatalf("debit to zero failed: %v balance=%d", err, w.Balance)
	}
	if err := w.Credit(0); !errors.Is(err, ErrValidation) {
		t.Fatalf("zero credit must fail, got %v", err)
	}
	if _, err := NewWallet("w", -1); !errors.Is(err, ErrValidation) {
		t.Fatalf("negative opening balance must fail, got %v", err)
	}
}
