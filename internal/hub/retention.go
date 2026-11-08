package hub

import (
	"context"
	"time"
)

// Compact drops old observation metadata, retaining identity tombstones and
// acquisition receipts: deleting those would allow delayed requests to revive
// completed runs. Each transaction processes at most 500 rows of each kind.
func (s *Store) Compact(ctx context.Context, retention time.Duration) error {
	if retention < 24*time.Hour {
		return ErrInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `WITH candidates AS (
 SELECT r.host_id,r.run_id FROM hrx_hub_runs r
 WHERE r.ended AND NOT r.archived
 AND r.received_at < clock_timestamp()-$1*interval '1 second'
 AND NOT EXISTS(SELECT 1 FROM hrx_hub_leases l WHERE l.host_id=r.host_id
 AND l.run_id=r.run_id AND l.expires_at>clock_timestamp())
 ORDER BY r.received_at LIMIT 500 FOR UPDATE OF r SKIP LOCKED
 )
 UPDATE hrx_hub_runs r SET archived=true,
 record=jsonb_build_object('agent',r.record->'agent','harness',r.record->'harness')
 FROM candidates c WHERE r.host_id=c.host_id AND r.run_id=c.run_id`, retention.Seconds()); err != nil {
		return err
	}
	// The latest receipt is retained indefinitely so a single pending outbox
	// remains retryable after a long outage. Older expired retries fail closed.
	if _, err = tx.Exec(ctx, `WITH candidates AS (
 SELECT r.host_id,r.sequence FROM hrx_hub_receipts r
 JOIN hrx_hub_hosts h USING(host_id)
 WHERE r.sequence<h.last_sequence
 AND r.received_at<clock_timestamp()-$1*interval '1 second'
 ORDER BY r.received_at LIMIT 500 FOR UPDATE OF r SKIP LOCKED
 )
 DELETE FROM hrx_hub_receipts r USING candidates c
 WHERE r.host_id=c.host_id AND r.sequence=c.sequence`, retention.Seconds()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
