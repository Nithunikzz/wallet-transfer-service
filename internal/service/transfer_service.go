// Package service holds the business workflow: idempotency, the transfer
// state machine and the orchestration of persistence in transactions.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/wallet-transfer-service/internal/domain"
)

// TransferResult is the outcome of CreateTransfer.
type TransferResult struct {
	Transfer domain.Transfer
	// Replayed is true when the idempotency key had already been used and
	// the result belongs to the original request.
	Replayed bool
}

// TransferService implements the wallet transfer use cases.
type TransferService struct {
	store Store
	log   *slog.Logger
	newID func() string
	// workTimeout bounds a transfer once it has been accepted for work,
	// independently of the caller's context.
	workTimeout time.Duration
}

// DefaultWorkTimeout is kept below the HTTP server's write timeout.
const DefaultWorkTimeout = 10 * time.Second

// New builds a TransferService. A nil logger discards logs.
func New(store Store, log *slog.Logger) *TransferService {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &TransferService{
		store:       store,
		log:         log,
		newID:       func() string { return uuid.NewString() },
		workTimeout: DefaultWorkTimeout,
	}
}

// errKeyTaken aborts the accept transaction when the idempotency key exists.
var errKeyTaken = errors.New("idempotency key already used")

// CreateTransfer executes a transfer with exactly-once semantics per
// idempotency key. See docs/design.md §4 for the workflow.
func (s *TransferService) CreateTransfer(ctx context.Context, req domain.TransferRequest) (TransferResult, error) {
	if err := req.Validate(); err != nil {
		return TransferResult{}, err
	}

	// A client disconnect must not abandon a transfer half-way (between Tx 1
	// and Tx 2 it would be stranded in PENDING, and a keyless request could
	// never be resumed). Detach from the caller's cancellation but keep its
	// values (request id), and bound the work with our own timeout instead.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.workTimeout)
	defer cancel()

	transferID, replayed, err := s.accept(ctx, req)
	if err != nil {
		if errors.Is(err, domain.ErrIdempotencyKeyReused) {
			// The key itself is client-controlled and may be sensitive: never log it.
			s.log.WarnContext(ctx, "idempotency key reused with different payload",
				"from_wallet", req.FromWalletID, "to_wallet", req.ToWalletID, "amount", req.Amount)
		}
		return TransferResult{}, err
	}
	t, err := s.process(ctx, transferID)
	if err != nil {
		s.log.ErrorContext(ctx, "transfer processing error", "transfer_id", transferID, "err", err)
		return TransferResult{}, err
	}
	s.logOutcome(ctx, t, replayed)
	return TransferResult{Transfer: t, Replayed: replayed}, nil
}

func (s *TransferService) logOutcome(ctx context.Context, t domain.Transfer, replayed bool) {
	attrs := []any{
		"transfer_id", t.ID,
		"from_wallet", t.FromWalletID,
		"to_wallet", t.ToWalletID,
		"amount", t.Amount,
		"status", t.Status,
		"replayed", replayed,
	}
	switch t.Status {
	case domain.TransferProcessed:
		s.log.InfoContext(ctx, "transfer processed", attrs...)
	case domain.TransferFailed:
		s.log.InfoContext(ctx, "transfer failed", append(attrs, "reason", t.FailureReason)...)
	}
}

// accept (Tx 1) persists the PENDING transfer and claims the idempotency key.
// When the key is already claimed it returns the existing transfer id.
func (s *TransferService) accept(ctx context.Context, req domain.TransferRequest) (string, bool, error) {
	t, err := domain.NewTransfer(s.newID(), req)
	if err != nil {
		return "", false, err
	}
	hash := req.Fingerprint()

	err = s.store.WithTx(ctx, func(tx Tx) error {
		if err := tx.EnsureWalletsExist(ctx, req.FromWalletID, req.ToWalletID); err != nil {
			return err
		}
		if err := tx.InsertTransfer(ctx, &t); err != nil {
			return err
		}
		if req.IdempotencyKey == "" {
			return nil
		}
		inserted, err := tx.InsertIdempotencyRecord(ctx, IdempotencyRecord{
			Key: req.IdempotencyKey, RequestHash: hash, TransferID: t.ID,
		})
		if err != nil {
			return err
		}
		if !inserted {
			return errKeyTaken // roll back our PENDING row
		}
		return nil
	})
	if err == nil {
		return t.ID, false, nil
	}
	if !errors.Is(err, errKeyTaken) {
		return "", false, err
	}

	// The key exists and its owning transaction has committed (the insert
	// waited for it). Adopt the existing transfer if the payload matches.
	var rec IdempotencyRecord
	err = s.store.WithTx(ctx, func(tx Tx) error {
		var err error
		rec, err = tx.GetIdempotencyRecord(ctx, req.IdempotencyKey)
		return err
	})
	if err != nil {
		return "", false, fmt.Errorf("load idempotency record: %w", err)
	}
	if rec.RequestHash != hash {
		return "", false, domain.ErrIdempotencyKeyReused
	}
	return rec.TransferID, true, nil
}

// process (Tx 2) drives a PENDING transfer to a terminal state exactly once.
// It is safe to call any number of times, concurrently, for the same id.
func (s *TransferService) process(ctx context.Context, transferID string) (domain.Transfer, error) {
	var out domain.Transfer
	err := s.store.WithTx(ctx, func(tx Tx) error {
		t, err := tx.GetTransferForUpdate(ctx, transferID)
		if err != nil {
			return err
		}
		if t.Status.IsTerminal() {
			out = t // already done by an earlier/concurrent request: replay
			return nil
		}

		wallets, err := tx.LockWallets(ctx, t.FromWalletID, t.ToWalletID)
		if err != nil {
			return err
		}
		from, to := wallets[t.FromWalletID], wallets[t.ToWalletID]

		switch err := from.Debit(t.Amount); {
		case errors.Is(err, domain.ErrInsufficientFunds):
			if err := t.MarkFailed(domain.FailureInsufficientFunds); err != nil {
				return err
			}
			if err := tx.CompletePendingTransfer(ctx, &t); err != nil {
				return err
			}
			out = t
			return nil
		case err != nil:
			return err
		}
		if err := to.Credit(t.Amount); err != nil {
			return err
		}
		if err := t.MarkProcessed(); err != nil {
			return err
		}

		if err := tx.UpdateWalletBalance(ctx, from); err != nil {
			return err
		}
		if err := tx.UpdateWalletBalance(ctx, to); err != nil {
			return err
		}
		entries := t.LedgerEntries()
		if err := tx.InsertLedgerEntries(ctx, entries[:]); err != nil {
			return err
		}
		if err := tx.CompletePendingTransfer(ctx, &t); err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

// GetTransfer returns a transfer by id.
func (s *TransferService) GetTransfer(ctx context.Context, id string) (domain.Transfer, error) {
	if _, err := uuid.Parse(id); err != nil {
		return domain.Transfer{}, domain.ErrTransferNotFound
	}
	return s.store.GetTransfer(ctx, id)
}

// CreateWallet creates a wallet with an opening balance.
func (s *TransferService) CreateWallet(ctx context.Context, id string, initialBalance int64) (domain.Wallet, error) {
	w, err := domain.NewWallet(id, initialBalance)
	if err != nil {
		return domain.Wallet{}, err
	}
	if err := s.store.CreateWallet(ctx, &w); err != nil {
		return domain.Wallet{}, err
	}
	return w, nil
}

// GetWallet returns a wallet by id.
func (s *TransferService) GetWallet(ctx context.Context, id string) (domain.Wallet, error) {
	return s.store.GetWallet(ctx, id)
}

// ListLedgerEntries returns a wallet's ledger entries, oldest first.
func (s *TransferService) ListLedgerEntries(ctx context.Context, walletID string) ([]domain.LedgerEntry, error) {
	if _, err := s.store.GetWallet(ctx, walletID); err != nil {
		return nil, err
	}
	return s.store.ListLedgerEntries(ctx, walletID)
}
