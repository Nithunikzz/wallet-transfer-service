package domain

import (
	"fmt"
	"regexp"
	"time"
)

// Wallet holds a stored balance in minor currency units.
type Wallet struct {
	ID             string
	Balance        int64
	OpeningBalance int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const (
	maxWalletIDLength       = 64
	maxIdempotencyKeyLength = 255
)

// Wallet ids are opaque but restricted to a URL- and log-safe alphabet.
var walletIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Idempotency keys are client-generated (often UUIDs): visible ASCII only.
var idempotencyKeyPattern = regexp.MustCompile(`^[\x21-\x7E]+$`)

// NewWallet validates and builds a wallet with an opening balance.
func NewWallet(id string, openingBalance int64) (Wallet, error) {
	if err := validateID("id", id); err != nil {
		return Wallet{}, err
	}
	if openingBalance < 0 {
		return Wallet{}, fmt.Errorf("%w: initialBalance must be >= 0", ErrValidation)
	}
	return Wallet{ID: id, Balance: openingBalance, OpeningBalance: openingBalance}, nil
}

// Debit removes amount from the wallet, refusing to go negative.
func (w *Wallet) Debit(amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("%w: debit amount must be positive", ErrValidation)
	}
	if w.Balance < amount {
		return ErrInsufficientFunds
	}
	w.Balance -= amount
	return nil
}

// Credit adds amount to the wallet.
func (w *Wallet) Credit(amount int64) error {
	if amount <= 0 {
		return fmt.Errorf("%w: credit amount must be positive", ErrValidation)
	}
	w.Balance += amount
	return nil
}

func validateID(field, id string) error {
	if id == "" {
		return fmt.Errorf("%w: %s is required", ErrValidation, field)
	}
	if len(id) > maxWalletIDLength {
		return fmt.Errorf("%w: %s must be at most %d characters", ErrValidation, field, maxWalletIDLength)
	}
	if !walletIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %s may contain only letters, digits, '_' and '-'", ErrValidation, field)
	}
	return nil
}

// validateIdempotencyKey accepts an empty key (the key is optional).
func validateIdempotencyKey(key string) error {
	if key == "" {
		return nil
	}
	if len(key) > maxIdempotencyKeyLength {
		return fmt.Errorf("%w: idempotencyKey must be at most %d characters", ErrValidation, maxIdempotencyKeyLength)
	}
	if !idempotencyKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: idempotencyKey must be visible ASCII without spaces", ErrValidation)
	}
	return nil
}
