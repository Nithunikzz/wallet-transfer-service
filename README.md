# Wallet Transfer Service

A Go + PostgreSQL service for wallet-to-wallet transfers. It covers idempotent
requests, a double-entry ledger, a transfer state machine, and safe concurrent
execution.

The full design (API contract, workflow, failure modes and retry behaviour) is
in [docs/design.md](docs/design.md). This README is a summary.

## Run

```bash
docker compose up --build          # API on :8080, Postgres on :5432
```

Or against your own Postgres:

```bash
DATABASE_URL=postgres://user:pass@localhost:5432/wallet?sslmode=disable go run ./cmd/server
```

The schema is applied at startup.

```bash
curl -XPOST localhost:8080/wallets   -d '{"id":"wallet_1","initialBalance":1000}'
curl -XPOST localhost:8080/wallets   -d '{"id":"wallet_2","initialBalance":0}'
curl -XPOST localhost:8080/transfers -d '{"idempotencyKey":"abc123","fromWalletId":"wallet_1","toWalletId":"wallet_2","amount":100}'
curl localhost:8080/wallets/wallet_1
curl localhost:8080/wallets/wallet_1/ledger
```

## Test

```bash
go test ./...
```

The integration tests (`internal/integration`) run against a **real
PostgreSQL**, because the concurrency guarantees depend on Postgres locking and
can't be shown with a mock. With no other setup, the tests download and start
an embedded PostgreSQL 16 (cached in `~/.embedded-postgres-go`). To use an
existing database instead, set `TEST_DATABASE_URL`. The tests truncate its
tables.

| Area | Scenarios |
|------|-----------|
| Idempotency | same key + same payload (replay); same key + different payload (`409`); 30 simultaneous requests with one key; client disconnects mid-request (with and without key: the transfer still completes, nothing left `PENDING`); replaying a `FAILED` result |
| Recovery | process crash while `PENDING`, finished by a **client retry with the same key** (no automatic recovery); **injected DB failure** after the debit, between the DEBIT and CREDIT inserts, and at the status update, plus a **real connection kill** (`pg_terminate_backend`) mid-transaction. Each case asserts a full rollback, then a successful retry |
| Concurrency | 30 concurrent debits against a balance that covers only 10 (no double spend); 40 opposite-direction transfers (no deadlock). Repeated 20× when the deadlock fix was made |
| Validation | same wallet, zero or negative amount, missing source or destination wallet (nothing persisted); invalid, float, string, oversized and trailing JSON; bad ID characters |
| Ledger | after every scenario, six named checks over the whole DB: exactly 2 entries per `PROCESSED` transfer; one DEBIT from the source and one CREDIT to the destination; entry amount equals transfer amount; `FAILED`/`PENDING` transfers have no entries; Σdebit = Σcredit; balance = opening + credits − debits |
| Schema backstop | the DB itself rejects a negative balance, a second DEBIT, a zero amount, an unknown status, `FAILED` without a reason, and a self-transfer |
| HTTP | status mapping, replay header, request-ID propagation, JSON `404`/`405`, panic recovery |

Run the concurrency tests repeatedly with
`go test -count=20 -run Concurrent ./internal/integration/`.

## Layout

```
cmd/server                     wiring, config, graceful shutdown
internal/domain                entities, validation, state machine, errors
internal/service               transfer workflow, idempotency; persistence ports (Store/Tx)
internal/repository/postgres   SQL implementation of the ports + embedded migrations
internal/handler               HTTP decoding, wire DTOs, error → status mapping, middleware
internal/logging               slog handler that attaches request_id from context
internal/integration           tests against real Postgres (incl. fault injection)
docs/design.md                 documentation-first design, with architecture and sequence diagrams
```

**Money.** Amounts are `int64` in minor units of a single currency (e.g.
`100` = ₹1.00). There are no floats anywhere, and JSON floats or strings are
rejected.

**Observability.** The service writes JSON logs (`log/slog`) and gives every
request an `X-Request-ID`, which is attached to all log lines for that request.
Idempotency keys and request bodies are never logged. See design.md §9.

The service depends only on the `Store`/`Tx` interfaces. Business rules live
in the domain:

- `Wallet.Debit` refuses to go negative.
- `Transfer.MarkProcessed` and `MarkFailed` enforce the state machine.
- `Transfer.LedgerEntries` builds the balanced entry pair.

The repository only persists data. It also enforces the same invariants with
database constraints, as a second layer of protection.

## Database schema

| Table | Purpose / key constraints |
|-------|---------------------------|
| `wallets` | Stored `balance` with `CHECK (balance >= 0)`. `opening_balance` is kept for reconciliation. |
| `transfers` | `status ∈ {PENDING, PROCESSED, FAILED}`. `CHECK (amount > 0)`, `from <> to`. `failure_reason` is set if and only if the status is `FAILED`. A partial index on `PENDING` rows supports a recovery sweeper. |
| `idempotency_records` | `key` PK → `request_hash`, `transfer_id` (unique FK). |
| `ledger_entries` | `DEBIT`/`CREDIT`, `CHECK (amount > 0)`. `UNIQUE (transfer_id, type)`: a transfer has at most one debit and one credit. |

Balances are **stored** rather than derived. Reads are O(1), and the wallet row
is a natural target for locking. The ledger is the audit trail, and every
wallet reconciles with it:
`balance = opening_balance + Σcredit − Σdebit`. The tests check this.

## Idempotency strategy

- The key is claimed with `INSERT … ON CONFLICT (key) DO NOTHING` in the same
  transaction that creates the `PENDING` transfer. Postgres makes a concurrent
  insert of the same key **wait** until the first transaction finishes, so
  exactly one transfer row survives for each key.
- The key is bound to a SHA-256 fingerprint of `(from, to, amount)`. If the
  same key arrives with a different payload, the service returns `409`.
- A replay returns the stored transfer with the original status code and an
  `Idempotent-Replayed: true` header. Terminal transfers never change, so a
  replay is deterministic. A transfer that `FAILED` for insufficient funds
  stays failed for that key.
- Validation errors and missing wallets are rejected **before** anything is
  persisted, so they don't use up the key.
- Requests without a key are processed normally and are not deduplicated.

## Concurrency handling

The service uses **pessimistic row locks in `READ COMMITTED`**:

1. `SELECT … FROM transfers … FOR NO KEY UPDATE` serialises everything that processes
   the same transfer. Whoever runs second sees a terminal state and replays it.
2. `SELECT … FROM wallets WHERE id = ANY(…) ORDER BY id FOR NO KEY UPDATE` locks both
   wallets in a fixed global order. This prevents double spending, because
   the balance is checked and debited under the lock. It also prevents
   deadlocks between A→B and B→A transfers. The lock mode is
   `FOR NO KEY UPDATE`, not `FOR UPDATE`. `FOR UPDATE` conflicts with the
   `FOR KEY SHARE` locks that foreign-key checks take on wallet rows, and an
   early version deadlocked under stress because of that (see design.md §7).
3. The balance updates, both ledger entries and the status change all commit in
   one transaction. The status update is guarded
   (`WHERE status = 'PENDING'`).

Optimistic locking was not used because wallet rows are hot: under contention
it causes many conflicts and application-level retries. `SERIALIZABLE` was not
used because it needs a retry loop for `40001` errors and adds no safety over
explicit locks for this access pattern.

## Assumptions & trade-offs

- **Currency: INR only.** Amounts are `int64` paise. Floats are rejected.
  Cross-currency transfers are unsupported. Supporting them would mean storing
  a currency on each wallet and validating that the two wallets match.
- **Trusted caller.** Authentication and authorization are not implemented,
  and any caller can debit any wallet. A production system must verify that
  the caller is allowed to debit the source wallet, and must scope idempotency
  keys per caller.
- **Two short transactions** (accept, then process) instead of one. This keeps
  `PENDING` meaningful: it is a durable record of an accepted request whose
  money hasn't moved yet.
- **Client disconnects don't abandon work.** After validation, the transfer
  runs on a context detached from the request, with a 10 s timeout.
- **Process-crash recovery is client-driven, not automatic.** If the process
  dies between the two transactions, the transfer stays `PENDING` with
  balances untouched. Nothing is lost or duplicated, but nothing moves until
  **the client retries with the same key**, which finishes it exactly once.
  Without a retry, or for a keyless transfer, it stays `PENDING`. Automatic
  recovery would need a background sweeper that re-runs `process` on old
  `PENDING` rows. It isn't implemented, but it would be safe: `process` locks
  the transfer and does nothing to a terminal one.
- **`POST /wallets` with `initialBalance`** stands in for a real funding
  flow.
- Idempotency records have no TTL.
- **`go test -race` has not been run.** The race detector needs cgo and a C
  compiler, and the dev machine has neither. The concurrency tests do use real
  goroutines against a real database. Run `go test -race ./...` on a machine
  with gcc before relying on it.
