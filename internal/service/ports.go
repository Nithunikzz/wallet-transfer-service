package service

import (
	"context"

	"github.com/wallet-transfer-service/internal/domain"
)

// IdempotencyRecord binds an idempotency key to a request fingerprint and the
// transfer it produced.
type IdempotencyRecord struct {
	Key         string
	RequestHash string
	TransferID  string
}

// Store is the persistence port the service depends on. Implementations must
// run every Tx in a single database transaction.
type Store interface {
	// WithTx runs fn inside one transaction; it commits if fn returns nil and
	// rolls back otherwise. The error returned by fn is returned unchanged.
	WithTx(ctx context.Context, fn func(Tx) error) error

	CreateWallet(ctx context.Context, w *domain.Wallet) error
	GetWallet(ctx context.Context, id string) (domain.Wallet, error)
	GetTransfer(ctx context.Context, id string) (domain.Transfer, error)
	ListLedgerEntries(ctx context.Context, walletID string) ([]domain.LedgerEntry, error)
}

// Tx is the set of operations available inside a transaction.
type Tx interface {
	// EnsureWalletsExist returns domain.ErrWalletNotFound if any id is unknown.
	EnsureWalletsExist(ctx context.Context, ids ...string) error
	// InsertTransfer persists a new transfer and fills its timestamps.
	InsertTransfer(ctx context.Context, t *domain.Transfer) error
	// InsertIdempotencyRecord inserts the record unless the key already
	// exists. It reports whether a row was inserted. If a concurrent
	// transaction holds the same key uncommitted, it blocks until that
	// transaction finishes.
	InsertIdempotencyRecord(ctx context.Context, rec IdempotencyRecord) (bool, error)
	GetIdempotencyRecord(ctx context.Context, key string) (IdempotencyRecord, error)
	// GetTransferForUpdate loads a transfer and row-locks it until the end of
	// the transaction.
	GetTransferForUpdate(ctx context.Context, id string) (domain.Transfer, error)
	// LockWallets row-locks the given wallets in ascending id order (to avoid
	// deadlocks) and returns them keyed by id.
	LockWallets(ctx context.Context, ids ...string) (map[string]*domain.Wallet, error)
	UpdateWalletBalance(ctx context.Context, w *domain.Wallet) error
	InsertLedgerEntries(ctx context.Context, entries []domain.LedgerEntry) error
	// CompletePendingTransfer persists a terminal status. It must only update
	// a row that is still PENDING and return domain.ErrInvalidTransition
	// otherwise.
	CompletePendingTransfer(ctx context.Context, t *domain.Transfer) error
}
