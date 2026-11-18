package domain

import "errors"

var (
	// ErrValidation wraps every request/entity validation failure.
	ErrValidation = errors.New("validation error")
	// ErrWalletNotFound is returned when a referenced wallet does not exist.
	ErrWalletNotFound = errors.New("wallet not found")
	// ErrWalletExists is returned when creating a wallet whose id is taken.
	ErrWalletExists = errors.New("wallet already exists")
	// ErrTransferNotFound is returned when a transfer id is unknown.
	ErrTransferNotFound = errors.New("transfer not found")
	// ErrIdempotencyKeyReused is returned when an idempotency key is replayed
	// with a payload different from the one it was first used with.
	ErrIdempotencyKeyReused = errors.New("idempotency key reused with a different request")
	// ErrInvalidTransition is returned for a disallowed transfer state change.
	ErrInvalidTransition = errors.New("invalid transfer state transition")
	// ErrInsufficientFunds is returned when a debit would make a balance negative.
	ErrInsufficientFunds = errors.New("insufficient funds")
)
