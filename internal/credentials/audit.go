package credentials

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var ErrAuditUnavailable = errors.New("credential audit write failed")

func writeCredentialAudit(ctx context.Context, tx pgx.Tx, actorID, action, target string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO audit_events(actor_user_id,action,target,succeeded) VALUES($1,$2,$3,true)`, actorID, action, target); err != nil {
		return fmt.Errorf("%w: %v", ErrAuditUnavailable, err)
	}
	return nil
}
