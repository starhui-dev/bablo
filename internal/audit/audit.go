// Package audit owns append-only security and administration audit records.
package audit

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/starhui-dev/bablo/internal/data"
	"github.com/starhui-dev/bablo/internal/id"
)

// Event is a sanitized audit fact. Callers must not place secrets or request
// bodies in any field.
type Event struct {
	ActorUserID *uuid.UUID
	Action      string
	TargetType  string
	TargetID    string
	RequestID   string
	Result      string
}

// Insert appends one audit event using the caller's transaction boundary.
func Insert(ctx context.Context, q data.Querier, event Event) error {
	if q == nil {
		return errors.New("audit insert requires a database queryer")
	}
	if event.Action == "" || event.TargetType == "" || event.Result == "" {
		return errors.New("audit event requires action, target type, and result")
	}

	auditID, err := id.New()
	if err != nil {
		return fmt.Errorf("generate audit UUIDv7: %w", err)
	}
	targetID := event.TargetID
	if targetID == "" {
		targetID = "unknown"
	}
	var requestID any
	if event.RequestID != "" {
		requestID = event.RequestID
	}
	if _, err := q.Exec(ctx, `
		INSERT INTO audit_logs (
			id, event_id, actor_user_id, action, target_type, target_id, request_id, result
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		auditID,
		"evt_"+auditID.String(),
		event.ActorUserID,
		event.Action,
		event.TargetType,
		targetID,
		requestID,
		event.Result,
	); err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}
