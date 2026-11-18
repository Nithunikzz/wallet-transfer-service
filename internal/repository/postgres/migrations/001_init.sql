-- Wallet balances are stored (not derived) and are the lock target for
-- transfers. The ledger is the audit trail:
--   balance = opening_balance + SUM(credits) - SUM(debits)
CREATE TABLE IF NOT EXISTS wallets (
    id              TEXT        PRIMARY KEY,
    balance         BIGINT      NOT NULL CHECK (balance >= 0),
    opening_balance BIGINT      NOT NULL CHECK (opening_balance >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS transfers (
    id             UUID        PRIMARY KEY,
    from_wallet_id TEXT        NOT NULL REFERENCES wallets (id),
    to_wallet_id   TEXT        NOT NULL REFERENCES wallets (id),
    amount         BIGINT      NOT NULL CHECK (amount > 0),
    status         TEXT        NOT NULL CHECK (status IN ('PENDING', 'PROCESSED', 'FAILED')),
    failure_reason TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT transfers_distinct_wallets CHECK (from_wallet_id <> to_wallet_id),
    CONSTRAINT transfers_failure_reason CHECK ((status = 'FAILED') = (failure_reason IS NOT NULL))
);

-- Supports a recovery sweeper for transfers stuck in PENDING.
CREATE INDEX IF NOT EXISTS transfers_pending_idx ON transfers (created_at) WHERE status = 'PENDING';

-- One row per idempotency key. The primary key is what serialises concurrent
-- requests carrying the same key.
CREATE TABLE IF NOT EXISTS idempotency_records (
    key          TEXT        PRIMARY KEY,
    request_hash TEXT        NOT NULL,
    transfer_id  UUID        NOT NULL UNIQUE REFERENCES transfers (id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Double-entry ledger. UNIQUE (transfer_id, type) means a transfer can never
-- have more than one DEBIT and one CREDIT, even if processing ran twice.
CREATE TABLE IF NOT EXISTS ledger_entries (
    id          BIGSERIAL   PRIMARY KEY,
    wallet_id   TEXT        NOT NULL REFERENCES wallets (id),
    transfer_id UUID        NOT NULL REFERENCES transfers (id),
    type        TEXT        NOT NULL CHECK (type IN ('DEBIT', 'CREDIT')),
    amount      BIGINT      NOT NULL CHECK (amount > 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ledger_entries_one_per_side UNIQUE (transfer_id, type)
);

CREATE INDEX IF NOT EXISTS ledger_entries_wallet_idx ON ledger_entries (wallet_id, id);
