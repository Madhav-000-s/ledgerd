package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/postgres/sqlc"
)

// LedgerRepo implements ledger.Repository against PostgreSQL.
//
// Every method resolves its querier from the context, so the same code runs inside a
// transaction opened by TxManager or standalone against the pool. Nothing here opens a
// transaction of its own: composing several writes into one atomic unit is the service
// layer's job, and is what lets the ledger write, the outbox insert and the idempotency
// record commit together.
type LedgerRepo struct {
	db *DB
}

var _ ledger.Repository = (*LedgerRepo)(nil)

// NewLedgerRepo returns a repository backed by db.
func NewLedgerRepo(db *DB) *LedgerRepo { return &LedgerRepo{db: db} }

// q returns the generated queries bound to this call's transaction, if there is one.
func (r *LedgerRepo) q(ctx context.Context) *sqlc.Queries {
	return sqlc.New(r.db.q(ctx))
}

// Postgres error codes worth distinguishing.
const (
	codeUniqueViolation = "23505"
	codeCheckViolation  = "23514"
)

// ── accounts ────────────────────────────────────────────────────────────────

func (r *LedgerRepo) CreateAccount(ctx context.Context, a *ledger.Account) error {
	metadata, err := marshalMetadata(a.Metadata)
	if err != nil {
		return err
	}

	err = r.q(ctx).CreateAccount(ctx, sqlc.CreateAccountParams{
		ID:            a.ID,
		MerchantID:    a.MerchantID,
		Name:          a.Name,
		Type:          sqlc.AccountType(a.Type),
		NormalBalance: sqlc.BalanceSide(a.NormalBalance),
		Currency:      string(a.Currency),
		AllowNegative: a.AllowNegative,
		Metadata:      metadata,
		CreatedAt:     timestamptz(a.CreatedAt),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == codeUniqueViolation {
			return fmt.Errorf("%w: %s/%s", ledger.ErrAccountExists, a.MerchantID, a.Name)
		}
		return fmt.Errorf("postgres: insert account: %w", err)
	}
	return nil
}

func (r *LedgerRepo) GetAccount(ctx context.Context, accountID string) (*ledger.Account, error) {
	row, err := r.q(ctx).GetAccount(ctx, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ledger.ErrAccountNotFound, accountID)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: select account: %w", err)
	}

	return accountFromRow(row.ID, row.MerchantID, row.Name, row.Type, row.NormalBalance,
		row.Currency, row.AllowNegative, row.Metadata, row.CreatedAt)
}

func (r *LedgerRepo) ListAccounts(ctx context.Context, merchantID string) ([]*ledger.Account, error) {
	rows, err := r.q(ctx).ListAccounts(ctx, merchantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list accounts: %w", err)
	}

	out := make([]*ledger.Account, 0, len(rows))
	for _, row := range rows {
		a, err := accountFromRow(row.ID, row.MerchantID, row.Name, row.Type, row.NormalBalance,
			row.Currency, row.AllowNegative, row.Metadata, row.CreatedAt)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// LockAccounts loads the named accounts and takes a row lock on each balance.
//
// The ordering that makes deadlock structurally impossible lives in the query itself
// (ORDER BY before FOR UPDATE); the caller having sorted the ids is a second statement
// of the same intent, in the place a reader is most likely to look.
func (r *LedgerRepo) LockAccounts(ctx context.Context, ids []string) (map[string]ledger.LockedAccount, error) {
	if len(ids) == 0 {
		return map[string]ledger.LockedAccount{}, nil
	}

	rows, err := r.q(ctx).LockAccounts(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("postgres: lock accounts: %w", err)
	}

	out := make(map[string]ledger.LockedAccount, len(ids))
	for _, row := range rows {
		a, err := accountFromRow(row.ID, row.MerchantID, row.Name, row.Type, row.NormalBalance,
			row.Currency, row.AllowNegative, row.Metadata, row.CreatedAt)
		if err != nil {
			return nil, err
		}

		out[a.ID] = ledger.LockedAccount{
			Account: *a,
			Balance: ledger.Balance{
				AccountID: a.ID,
				Currency:  a.Currency,
				Posted:    row.PostedMinor,
				Pending:   row.PendingMinor,
				Version:   row.Version,
				UpdatedAt: row.UpdatedAt.Time,
			},
		}
	}

	// A missing id means the caller named an account that does not exist. Failing here
	// rather than later keeps the error precise and the transaction short.
	for _, wanted := range ids {
		if _, ok := out[wanted]; !ok {
			return nil, fmt.Errorf("%w: %s", ledger.ErrAccountNotFound, wanted)
		}
	}
	return out, nil
}

func (r *LedgerRepo) ApplyBalances(ctx context.Context, updates []ledger.BalanceUpdate) error {
	q := r.q(ctx)

	for _, u := range updates {
		affected, err := q.ApplyBalance(ctx, sqlc.ApplyBalanceParams{
			AccountID:    u.AccountID,
			PostedMinor:  u.Posted,
			PendingMinor: u.Pending,
			Version:      u.Version,
		})
		if err != nil {
			return fmt.Errorf("postgres: update balance %s: %w", u.AccountID, err)
		}
		if affected == 0 {
			// The row lock should have made this unreachable. Reaching it means the
			// locking contract was broken somewhere, which is worth an error rather
			// than a silently discarded write.
			return fmt.Errorf("postgres: balance of %s changed under the lock (expected version %d)",
				u.AccountID, u.Version)
		}
	}
	return nil
}

func (r *LedgerRepo) GetBalance(ctx context.Context, accountID string) (ledger.Balance, error) {
	row, err := r.q(ctx).GetBalance(ctx, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.Balance{}, fmt.Errorf("%w: %s", ledger.ErrAccountNotFound, accountID)
	}
	if err != nil {
		return ledger.Balance{}, fmt.Errorf("postgres: select balance: %w", err)
	}

	return ledger.Balance{
		AccountID: row.AccountID,
		Currency:  money.Currency(strings.TrimSpace(row.Currency)),
		Posted:    row.PostedMinor,
		Pending:   row.PendingMinor,
		Version:   row.Version,
		UpdatedAt: row.UpdatedAt.Time,
	}, nil
}

// ── transactions ────────────────────────────────────────────────────────────

func (r *LedgerRepo) InsertTransaction(ctx context.Context, t *ledger.Transaction) error {
	metadata, err := marshalMetadata(t.Metadata)
	if err != nil {
		return err
	}

	err = r.q(ctx).InsertTransaction(ctx, sqlc.InsertTransactionParams{
		ID:          t.ID,
		MerchantID:  t.MerchantID,
		Status:      sqlc.TxnStatus(t.Status),
		Currency:    string(t.Currency),
		Description: t.Description,
		ReversesID:  nullString(t.ReversesID),
		ExternalRef: nullString(t.ExternalRef),
		Metadata:    metadata,
		CreatedAt:   timestamptz(t.CreatedAt),
		PostedAt:    timestamptzPtr(t.PostedAt),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == codeUniqueViolation && t.ReversesID != "" {
			// The unique partial index on reverses_id is what actually stops two
			// concurrent refunds from both posting after both passed their
			// application-level check.
			return fmt.Errorf("%w: %s", ledger.ErrAlreadyReversed, t.ReversesID)
		}
		return fmt.Errorf("postgres: insert transaction: %w", err)
	}
	return nil
}

func (r *LedgerRepo) SetTransactionStatus(ctx context.Context, txnID string, status ledger.Status, postedAt *time.Time) error {
	affected, err := r.q(ctx).SetTransactionStatus(ctx, sqlc.SetTransactionStatusParams{
		ID:       txnID,
		Status:   sqlc.TxnStatus(status),
		PostedAt: timestamptzPtr(postedAt),
	})
	if err != nil {
		return fmt.Errorf("postgres: set transaction status: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("%w: %s", ledger.ErrTransactionNotFound, txnID)
	}
	return nil
}

func (r *LedgerRepo) GetTransaction(ctx context.Context, txnID string) (*ledger.Transaction, error) {
	row, err := r.q(ctx).GetTransaction(ctx, txnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ledger.ErrTransactionNotFound, txnID)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: select transaction: %w", err)
	}

	t, err := transactionFromRow(sqlc.Transaction(row))
	if err != nil {
		return nil, err
	}
	if t.Entries, err = r.EntriesByTransaction(ctx, txnID); err != nil {
		return nil, err
	}
	return t, nil
}

func (r *LedgerRepo) FindReversal(ctx context.Context, reversesID string) (*ledger.Transaction, error) {
	row, err := r.q(ctx).FindReversal(ctx, &reversesID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: nothing reverses %s", ledger.ErrTransactionNotFound, reversesID)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: find reversal: %w", err)
	}
	return transactionFromRow(sqlc.Transaction(row))
}

// TransactionsByRef returns every transaction belonging to one external object, so a
// payment's public state can be derived from the books rather than stored beside them.
func (r *LedgerRepo) TransactionsByRef(ctx context.Context, merchantID, externalRef string) ([]*ledger.Transaction, error) {
	rows, err := r.q(ctx).TransactionsByRef(ctx, sqlc.TransactionsByRefParams{
		MerchantID:  merchantID,
		ExternalRef: nullString(externalRef),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: transactions by ref: %w", err)
	}

	out := make([]*ledger.Transaction, 0, len(rows))
	for _, row := range rows {
		t, err := transactionFromRow(sqlc.Transaction(row))
		if err != nil {
			return nil, err
		}
		if t.Entries, err = r.EntriesByTransaction(ctx, t.ID); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// ── entries ─────────────────────────────────────────────────────────────────

// AppendEntries writes every entry of a transaction in one round trip.
//
// seq is echoed back with each generated id because RETURNING makes no promise about
// ordering, and each id has to be matched to the entry it belongs to.
func (r *LedgerRepo) AppendEntries(ctx context.Context, entries []ledger.Entry) ([]ledger.Entry, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	params := make([]sqlc.AppendEntryParams, len(entries))
	for i, e := range entries {
		params[i] = sqlc.AppendEntryParams{
			TransactionID: e.TransactionID,
			AccountID:     e.AccountID,
			Direction:     sqlc.BalanceSide(e.Direction),
			AmountMinor:   e.Amount,
			Currency:      string(e.Currency),
			Status:        sqlc.EntryStatus(e.Status),
			Seq:           e.Seq,
			BalanceAfter:  e.BalanceAfter,
			CreatedAt:     timestamptz(e.CreatedAt),
		}
	}

	type written struct {
		id        int64
		createdAt time.Time
	}
	bySeq := make(map[int16]written, len(entries))

	batch := r.q(ctx).AppendEntry(ctx, params)
	var batchErr error
	batch.QueryRow(func(_ int, row sqlc.AppendEntryRow, err error) {
		if err != nil {
			if batchErr == nil {
				batchErr = err
			}
			return
		}
		bySeq[row.Seq] = written{id: row.ID, createdAt: row.CreatedAt.Time}
	})
	if err := batch.Close(); err != nil && batchErr == nil {
		batchErr = err
	}
	if batchErr != nil {
		return nil, wrapEntryError(batchErr)
	}

	out := make([]ledger.Entry, len(entries))
	for i, e := range entries {
		w, ok := bySeq[e.Seq]
		if !ok {
			return nil, fmt.Errorf("postgres: entry seq %d was not returned by the insert", e.Seq)
		}
		e.ID = w.id
		e.CreatedAt = w.createdAt
		out[i] = e
	}
	return out, nil
}

func (r *LedgerRepo) ListEntries(ctx context.Context, accountID string, p ledger.Page) ([]ledger.Entry, string, error) {
	p = p.Normalize()

	var after *int64
	if p.StartingAfter != "" {
		c, err := ledger.ParseCursor(p.StartingAfter)
		if err != nil {
			return nil, "", err
		}
		after = &c.ID
	}

	// One extra row tells us whether a next page exists, without a second count query.
	rows, err := r.q(ctx).ListEntries(ctx, sqlc.ListEntriesParams{
		AccountID: accountID,
		AfterID:   after,
		Limit:     int32(p.Limit) + 1,
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list entries: %w", err)
	}

	out := make([]ledger.Entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, entryFromRow(row))
	}

	if len(out) > p.Limit {
		out = out[:p.Limit]
		return out, ledger.CursorFor(out[len(out)-1]).String(), nil
	}
	return out, "", nil
}

func (r *LedgerRepo) EntriesByTransaction(ctx context.Context, txnID string) ([]ledger.Entry, error) {
	rows, err := r.q(ctx).EntriesByTransaction(ctx, txnID)
	if err != nil {
		return nil, fmt.Errorf("postgres: entries by transaction: %w", err)
	}

	out := make([]ledger.Entry, 0, len(rows))
	for _, row := range rows {
		out = append(out, entryFromRow(row))
	}
	return out, nil
}

// ── conversions ─────────────────────────────────────────────────────────────

func accountFromRow(
	id, merchantID, name string,
	typ sqlc.AccountType,
	normal sqlc.BalanceSide,
	currency string,
	allowNegative bool,
	metadata []byte,
	createdAt pgtype.Timestamptz,
) (*ledger.Account, error) {
	m, err := unmarshalMetadata(metadata)
	if err != nil {
		return nil, err
	}
	return &ledger.Account{
		ID:            id,
		MerchantID:    merchantID,
		Name:          name,
		Type:          ledger.AccountType(typ),
		NormalBalance: ledger.Direction(normal),
		// CHAR(3) comes back blank-padded if a shorter value were ever stored; trimming
		// keeps a stray space out of a currency comparison.
		Currency:      money.Currency(strings.TrimSpace(currency)),
		AllowNegative: allowNegative,
		Metadata:      m,
		CreatedAt:     createdAt.Time,
	}, nil
}

func transactionFromRow(row sqlc.Transaction) (*ledger.Transaction, error) {
	m, err := unmarshalMetadata(row.Metadata)
	if err != nil {
		return nil, err
	}

	t := &ledger.Transaction{
		ID:          row.ID,
		MerchantID:  row.MerchantID,
		Status:      ledger.Status(row.Status),
		Currency:    money.Currency(strings.TrimSpace(row.Currency)),
		Description: row.Description,
		ReversesID:  derefString(row.ReversesID),
		ExternalRef: derefString(row.ExternalRef),
		Metadata:    m,
		CreatedAt:   row.CreatedAt.Time,
	}
	if row.PostedAt.Valid {
		at := row.PostedAt.Time
		t.PostedAt = &at
	}
	return t, nil
}

func entryFromRow(row sqlc.Entry) ledger.Entry {
	return ledger.Entry{
		ID:            row.ID,
		TransactionID: row.TransactionID,
		AccountID:     row.AccountID,
		Direction:     ledger.Direction(row.Direction),
		Amount:        row.AmountMinor,
		Currency:      money.Currency(strings.TrimSpace(row.Currency)),
		Status:        ledger.Status(row.Status),
		Seq:           row.Seq,
		BalanceAfter:  row.BalanceAfter,
		CreatedAt:     row.CreatedAt.Time,
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func marshalMetadata(m map[string]any) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("postgres: marshalling metadata: %w", err)
	}
	return b, nil
}

func unmarshalMetadata(b []byte) (map[string]any, error) {
	if len(b) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("postgres: unmarshalling metadata: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// nullString maps the domain's "unset" empty string onto SQL NULL, so the partial
// unique index on reverses_id behaves as intended.
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

func timestamptzPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// wrapEntryError turns the constraint trigger's check_violation into the domain error it
// corresponds to.
//
// The trigger is the layer that catches what the Go code missed, so letting its raw
// message reach a caller would leak the schema into the API. Each invariant raises a
// distinct message, which makes the mapping exact rather than a guess.
func wrapEntryError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("postgres: append entries: %w", err)
	}

	switch pgErr.Code {
	case codeCheckViolation:
		switch {
		case strings.Contains(pgErr.Message, "unbalanced"):
			return fmt.Errorf("%w: %s", ledger.ErrUnbalanced, pgErr.Message)
		case strings.Contains(pgErr.Message, "currencies"):
			return fmt.Errorf("%w: %s", ledger.ErrMultiCurrency, pgErr.Message)
		case strings.Contains(pgErr.Message, "no entries"):
			return fmt.Errorf("%w: %s", ledger.ErrNoEntries, pgErr.Message)
		case strings.Contains(pgErr.Message, "amount_minor"):
			return fmt.Errorf("%w: %s", ledger.ErrInvalidAmount, pgErr.Message)
		default:
			return fmt.Errorf("%w: %s", ledger.ErrUnbalanced, pgErr.Message)
		}
	case codeUniqueViolation:
		return fmt.Errorf("postgres: duplicate entry: %w", err)
	default:
		return fmt.Errorf("postgres: append entries: %w", err)
	}
}
