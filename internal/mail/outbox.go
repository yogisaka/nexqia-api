package mail

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Querier is satisfied by pgx.Tx and *pgxpool.Pool.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ClaimedEmail is one core.email_outbox row handed to the worker by core.claim_emails.
type ClaimedEmail struct {
	ID       pgtype.UUID
	To       string
	Subject  string
	Text     string
	HTML     string
	Attempts int32
	Kind     string
}

// ClaimEmails calls core.claim_emails (migrations/000061), a SECURITY DEFINER
// function that marks up to batch due rows as 'sending' for lockSeconds. A
// 'sending' row whose lock expired counts as a failed attempt and is given up
// (not returned) at maxAttempts — spec 2026-10-06-email-outbox-claim §2.2.
// sqlc can't type a RETURNS TABLE function call reliably, so this is a raw query.
func ClaimEmails(ctx context.Context, q Querier, batch, lockSeconds, maxAttempts int32) ([]ClaimedEmail, error) {
	rows, err := q.Query(ctx,
		"SELECT id, to_address, subject, body_text, body_html, attempts, kind FROM core.claim_emails($1, $2, $3)",
		batch, lockSeconds, maxAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClaimedEmail
	for rows.Next() {
		var e ClaimedEmail
		if err := rows.Scan(&e.ID, &e.To, &e.Subject, &e.Text, &e.HTML, &e.Attempts, &e.Kind); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
