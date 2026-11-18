// Package postgres implements the service persistence ports on PostgreSQL.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wallet-transfer-service/internal/domain"
	"github.com/wallet-transfer-service/internal/service"
)

//go:embed migrations/*.sql
var migrations embed.FS

const pgUniqueViolation = "23505"

// Store is a PostgreSQL-backed service.Store.
type Store struct {
	pool *pgxpool.Pool
}

var _ service.Store = (*Store)(nil)

// New wraps a connection pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Migrate applies the embedded schema. Statements are idempotent.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := migrations.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}
	return nil
}

// WithTx runs fn in a READ COMMITTED transaction. Correctness relies on
// explicit row locks (SELECT ... FOR NO KEY UPDATE), not on the isolation level.
func (s *Store) WithTx(ctx context.Context, fn func(service.Tx) error) (err error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err = fn(&pgTx{tx: tx}); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

// querier is satisfied by both the pool and a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (s *Store) CreateWallet(ctx context.Context, w *domain.Wallet) error {
	err := s.pool.QueryRow(ctx, `
		INSERT INTO wallets (id, balance, opening_balance)
		VALUES ($1, $2, $2)
		RETURNING created_at, updated_at`, w.ID, w.OpeningBalance,
	).Scan(&w.CreatedAt, &w.UpdatedAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return domain.ErrWalletExists
	}
	return err
}

func (s *Store) GetWallet(ctx context.Context, id string) (domain.Wallet, error) {
	var w domain.Wallet
	err := s.pool.QueryRow(ctx, `
		SELECT id, balance, opening_balance, created_at, updated_at
		FROM wallets WHERE id = $1`, id,
	).Scan(&w.ID, &w.Balance, &w.OpeningBalance, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, domain.ErrWalletNotFound
	}
	return w, err
}

func (s *Store) GetTransfer(ctx context.Context, id string) (domain.Transfer, error) {
	return getTransfer(ctx, s.pool, id, false)
}

func (s *Store) ListLedgerEntries(ctx context.Context, walletID string) ([]domain.LedgerEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, wallet_id, transfer_id, type, amount, created_at
		FROM ledger_entries WHERE wallet_id = $1 ORDER BY id`, walletID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (domain.LedgerEntry, error) {
		var e domain.LedgerEntry
		var typ string
		err := r.Scan(&e.ID, &e.WalletID, &e.TransferID, &typ, &e.Amount, &e.CreatedAt)
		e.Type = domain.EntryType(typ)
		return e, err
	})
}

const transferColumns = `
	t.id::text, COALESCE(i.key, ''), t.from_wallet_id, t.to_wallet_id, t.amount,
	t.status, COALESCE(t.failure_reason, ''), t.created_at, t.updated_at`

func getTransfer(ctx context.Context, q querier, id string, forUpdate bool) (domain.Transfer, error) {
	sql := `SELECT ` + transferColumns + `
		FROM transfers t
		LEFT JOIN idempotency_records i ON i.transfer_id = t.id
		WHERE t.id = $1`
	if forUpdate {
		sql += ` FOR NO KEY UPDATE OF t`
	}
	var t domain.Transfer
	var status, reason string
	err := q.QueryRow(ctx, sql, id).Scan(
		&t.ID, &t.IdempotencyKey, &t.FromWalletID, &t.ToWalletID, &t.Amount,
		&status, &reason, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, domain.ErrTransferNotFound
	}
	t.Status = domain.TransferStatus(status)
	t.FailureReason = domain.FailureReason(reason)
	return t, err
}

// pgTx implements service.Tx on a pgx transaction.
type pgTx struct {
	tx pgx.Tx
}

func (p *pgTx) EnsureWalletsExist(ctx context.Context, ids ...string) error {
	var n int
	if err := p.tx.QueryRow(ctx,
		`SELECT count(*) FROM wallets WHERE id = ANY($1)`, ids).Scan(&n); err != nil {
		return err
	}
	if n != len(unique(ids)) {
		return domain.ErrWalletNotFound
	}
	return nil
}

func (p *pgTx) InsertTransfer(ctx context.Context, t *domain.Transfer) error {
	return p.tx.QueryRow(ctx, `
		INSERT INTO transfers (id, from_wallet_id, to_wallet_id, amount, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at, updated_at`,
		t.ID, t.FromWalletID, t.ToWalletID, t.Amount, string(t.Status),
	).Scan(&t.CreatedAt, &t.UpdatedAt)
}

func (p *pgTx) InsertIdempotencyRecord(ctx context.Context, rec service.IdempotencyRecord) (bool, error) {
	tag, err := p.tx.Exec(ctx, `
		INSERT INTO idempotency_records (key, request_hash, transfer_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO NOTHING`, rec.Key, rec.RequestHash, rec.TransferID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (p *pgTx) GetIdempotencyRecord(ctx context.Context, key string) (service.IdempotencyRecord, error) {
	var r service.IdempotencyRecord
	err := p.tx.QueryRow(ctx, `
		SELECT key, request_hash, transfer_id::text
		FROM idempotency_records WHERE key = $1`, key,
	).Scan(&r.Key, &r.RequestHash, &r.TransferID)
	return r, err
}

func (p *pgTx) GetTransferForUpdate(ctx context.Context, id string) (domain.Transfer, error) {
	return getTransfer(ctx, p.tx, id, true)
}

func (p *pgTx) LockWallets(ctx context.Context, ids ...string) (map[string]*domain.Wallet, error) {
	ids = unique(ids)
	// ORDER BY id makes every transaction acquire row locks in the same
	// global order, which rules out lock-ordering deadlocks.
	rows, err := p.tx.Query(ctx, `
		SELECT id, balance, opening_balance, created_at, updated_at
		FROM wallets WHERE id = ANY($1)
		ORDER BY id
		FOR NO KEY UPDATE`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]*domain.Wallet, len(ids))
	for rows.Next() {
		var w domain.Wallet
		if err := rows.Scan(&w.ID, &w.Balance, &w.OpeningBalance, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		out[w.ID] = &w
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(ids) {
		return nil, domain.ErrWalletNotFound
	}
	return out, nil
}

func (p *pgTx) UpdateWalletBalance(ctx context.Context, w *domain.Wallet) error {
	return p.tx.QueryRow(ctx, `
		UPDATE wallets SET balance = $2, updated_at = now()
		WHERE id = $1
		RETURNING updated_at`, w.ID, w.Balance,
	).Scan(&w.UpdatedAt)
}

func (p *pgTx) InsertLedgerEntries(ctx context.Context, entries []domain.LedgerEntry) error {
	for i := range entries {
		e := &entries[i]
		if err := p.tx.QueryRow(ctx, `
			INSERT INTO ledger_entries (wallet_id, transfer_id, type, amount)
			VALUES ($1, $2, $3, $4)
			RETURNING id, created_at`,
			e.WalletID, e.TransferID, string(e.Type), e.Amount,
		).Scan(&e.ID, &e.CreatedAt); err != nil {
			return err
		}
	}
	return nil
}

func (p *pgTx) CompletePendingTransfer(ctx context.Context, t *domain.Transfer) error {
	var reason *string
	if t.FailureReason != domain.FailureNone {
		r := string(t.FailureReason)
		reason = &r
	}
	err := p.tx.QueryRow(ctx, `
		UPDATE transfers SET status = $2, failure_reason = $3, updated_at = now()
		WHERE id = $1 AND status = 'PENDING'
		RETURNING updated_at`, t.ID, string(t.Status), reason,
	).Scan(&t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: transfer %s is not PENDING", domain.ErrInvalidTransition, t.ID)
	}
	return err
}

func unique(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}
