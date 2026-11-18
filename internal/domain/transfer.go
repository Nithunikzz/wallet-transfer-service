package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// TransferStatus is the state of a transfer.
type TransferStatus string

const (
	TransferPending   TransferStatus = "PENDING"
	TransferProcessed TransferStatus = "PROCESSED"
	TransferFailed    TransferStatus = "FAILED"
)

// IsTerminal reports whether no further transitions are allowed.
func (s TransferStatus) IsTerminal() bool {
	return s == TransferProcessed || s == TransferFailed
}

// FailureReason explains why a transfer ended in FAILED.
type FailureReason string

const (
	FailureNone              FailureReason = ""
	FailureInsufficientFunds FailureReason = "INSUFFICIENT_FUNDS"
)

// TransferRequest is a validated intent to move money.
type TransferRequest struct {
	IdempotencyKey string // optional
	FromWalletID   string
	ToWalletID     string
	Amount         int64
}

// Validate checks the request against the domain rules.
func (r TransferRequest) Validate() error {
	if err := validateIdempotencyKey(r.IdempotencyKey); err != nil {
		return err
	}
	if err := validateID("fromWalletId", r.FromWalletID); err != nil {
		return err
	}
	if err := validateID("toWalletId", r.ToWalletID); err != nil {
		return err
	}
	if r.FromWalletID == r.ToWalletID {
		return fmt.Errorf("%w: fromWalletId and toWalletId must differ", ErrValidation)
	}
	if r.Amount <= 0 {
		return fmt.Errorf("%w: amount must be a positive integer", ErrValidation)
	}
	return nil
}

// Fingerprint is a stable hash of the fields that define the request's
// meaning. It binds an idempotency key to one specific payload.
func (r TransferRequest) Fingerprint() string {
	canonical := r.FromWalletID + "\x00" + r.ToWalletID + "\x00" + strconv.FormatInt(r.Amount, 10)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// Transfer is a money movement between two wallets with a state machine.
type Transfer struct {
	ID             string
	IdempotencyKey string
	FromWalletID   string
	ToWalletID     string
	Amount         int64
	Status         TransferStatus
	FailureReason  FailureReason
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewTransfer creates a PENDING transfer from a valid request.
func NewTransfer(id string, req TransferRequest) (Transfer, error) {
	if err := req.Validate(); err != nil {
		return Transfer{}, err
	}
	return Transfer{
		ID:             id,
		IdempotencyKey: req.IdempotencyKey,
		FromWalletID:   req.FromWalletID,
		ToWalletID:     req.ToWalletID,
		Amount:         req.Amount,
		Status:         TransferPending,
	}, nil
}

// MarkProcessed transitions PENDING -> PROCESSED.
func (t *Transfer) MarkProcessed() error {
	if t.Status != TransferPending {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.Status, TransferProcessed)
	}
	t.Status = TransferProcessed
	return nil
}

// MarkFailed transitions PENDING -> FAILED with a reason.
func (t *Transfer) MarkFailed(reason FailureReason) error {
	if t.Status != TransferPending {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, t.Status, TransferFailed)
	}
	if reason == FailureNone {
		return fmt.Errorf("%w: failure reason is required", ErrValidation)
	}
	t.Status = TransferFailed
	t.FailureReason = reason
	return nil
}

// EntryType is the side of a ledger entry.
type EntryType string

const (
	EntryDebit  EntryType = "DEBIT"
	EntryCredit EntryType = "CREDIT"
)

// LedgerEntry is one side of a double-entry posting.
type LedgerEntry struct {
	ID         int64
	WalletID   string
	TransferID string
	Type       EntryType
	Amount     int64
	CreatedAt  time.Time
}

// LedgerEntries returns the balanced DEBIT/CREDIT pair for a transfer.
func (t Transfer) LedgerEntries() [2]LedgerEntry {
	return [2]LedgerEntry{
		{WalletID: t.FromWalletID, TransferID: t.ID, Type: EntryDebit, Amount: t.Amount},
		{WalletID: t.ToWalletID, TransferID: t.ID, Type: EntryCredit, Amount: t.Amount},
	}
}
