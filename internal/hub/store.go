package hub

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/meshcrawler/hrx/internal/monitor"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	Pool       *pgxpool.Pool
	LeaseTTL   time.Duration
	StaleAfter time.Duration
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, errors.New("invalid hub database configuration")
	}
	s := &Store{Pool: p, LeaseTTL: 45 * time.Second, StaleAfter: 45 * time.Second}
	if err = p.Ping(ctx); err != nil {
		p.Close()
		return nil, errors.New("hub database unavailable")
	}
	if err = s.Migrate(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(193621001)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS hrx_hub_schema_migrations(version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		return err
	}
	var version int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0) FROM hrx_hub_schema_migrations`).Scan(&version); err != nil {
		return err
	}
	files := []string{"001_initial.sql", "002_retention.sql"}
	if version > len(files) {
		return errors.New("hub database schema is newer than this binary")
	}
	for index := version; index < len(files); index++ {
		data, err := migrations.ReadFile("migrations/" + files[index])
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(data)); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO hrx_hub_schema_migrations(version) VALUES($1)`, index+1); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) Enroll(ctx context.Context, e Enrollment) error {
	if err := e.validate(); err != nil {
		return err
	}
	e.Prefixes = slices.Compact(slices.Sorted(slices.Values(e.Prefixes)))
	prefixes, _ := json.Marshal(e.Prefixes)
	tag, err := s.Pool.Exec(ctx, `INSERT INTO hrx_hub_hosts(host_id,name,token_digest,prefixes) VALUES($1,$2,$3,$4)
ON CONFLICT(host_id) DO UPDATE SET name=EXCLUDED.name
WHERE hrx_hub_hosts.token_digest=EXCLUDED.token_digest AND hrx_hub_hosts.prefixes=EXCLUDED.prefixes AND NOT hrx_hub_hosts.revoked`, e.HostID, e.Name, e.TokenDigest, prefixes)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) Authenticate(ctx context.Context, token string) (Host, error) {
	if len(token) < 32 || len(token) > 512 {
		return Host{}, ErrUnauthorized
	}
	var h Host
	var prefixes []byte
	err := s.Pool.QueryRow(ctx, `SELECT host_id,name,prefixes,token_digest,last_sequence FROM hrx_hub_hosts WHERE token_digest=$1 AND NOT revoked`, Digest(token)).Scan(&h.ID, &h.Name, &prefixes, &h.TokenDigest, &h.LastSequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return Host{}, ErrUnauthorized
	}
	if err != nil {
		return Host{}, err
	}
	if json.Unmarshal(prefixes, &h.Prefixes) != nil {
		return Host{}, ErrUnauthorized
	}
	return h, nil
}

// Every mutation rechecks authorization while holding the host row lock, so
// credential revocation cannot race an already authenticated writer.
func lockHost(ctx context.Context, tx pgx.Tx, h Host) (Host, error) {
	var current Host
	var prefixes []byte
	err := tx.QueryRow(ctx, `SELECT host_id,name,prefixes,token_digest FROM hrx_hub_hosts WHERE host_id=$1 AND token_digest=$2 AND NOT revoked FOR UPDATE`, h.ID, h.TokenDigest).Scan(&current.ID, &current.Name, &prefixes, &current.TokenDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Host{}, ErrUnauthorized
	}
	if err != nil {
		return Host{}, err
	}
	if json.Unmarshal(prefixes, &current.Prefixes) != nil {
		return Host{}, ErrUnauthorized
	}
	return current, nil
}

func (s *Store) Revoke(ctx context.Context, id string) error {
	if !identifier.MatchString(id) {
		return ErrInvalid
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE hrx_hub_hosts SET revoked=true WHERE host_id=$1`, id); err != nil {
		return err
	}
	// Preserve expiry: a disconnected supervisor may still be within its lease.
	// Revocation rejects its next renewal; successors wait out that same deadline.
	return tx.Commit(ctx)
}

func (s *Store) Rotate(ctx context.Context, id, digest string) error {
	if !identifier.MatchString(id) || !digestPattern.MatchString(digest) {
		return ErrInvalid
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE hrx_hub_hosts SET token_digest=$2 WHERE host_id=$1 AND NOT revoked`, id, digest)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrUnauthorized
	}
	return nil
}

func (s *Store) Heartbeat(ctx context.Context, h Host, liveRuns []string) error {
	if len(liveRuns) > 1000 {
		return ErrInvalid
	}
	for _, id := range liveRuns {
		if !identifier.MatchString(id) {
			return ErrInvalid
		}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = lockHost(ctx, tx, h); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE hrx_hub_hosts SET last_heartbeat=clock_timestamp() WHERE host_id=$1`, h.ID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE hrx_hub_runs SET live_seen_at=clock_timestamp() WHERE host_id=$1 AND run_id=ANY($2) AND NOT ended`, h.ID, liveRuns); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Ingest(ctx context.Context, h Host, b Batch) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	h, err = lockHost(ctx, tx, h)
	if err != nil {
		return err
	}
	if err = b.validate(h); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return ErrInvalid
	}
	digest := Digest(string(data))
	var last int64
	if err = tx.QueryRow(ctx, `SELECT last_sequence FROM hrx_hub_hosts WHERE host_id=$1`, h.ID).Scan(&last); err != nil {
		return err
	}
	if b.Sequence <= last {
		var prior string
		err = tx.QueryRow(ctx, `SELECT payload_digest FROM hrx_hub_receipts WHERE host_id=$1 AND sequence=$2`, h.ID, b.Sequence).Scan(&prior)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && prior != digest {
			return ErrConflict
		}
		return err
	}
	for _, r := range b.Records {
		r.Host = h.Name
		data, _ := json.Marshal(r)
		// Identity and lifecycle cannot be rewritten even by a later batch.
		tag, err := tx.Exec(ctx, `INSERT INTO hrx_hub_runs(host_id,run_id,sequence,record,ended) VALUES($1,$2,$3,$4,$5)
ON CONFLICT(host_id,run_id) DO UPDATE SET sequence=EXCLUDED.sequence,record=EXCLUDED.record,ended=EXCLUDED.ended,received_at=clock_timestamp()
WHERE NOT hrx_hub_runs.ended AND hrx_hub_runs.record->>'agent'=EXCLUDED.record->>'agent' AND hrx_hub_runs.record->>'harness'=EXCLUDED.record->>'harness'`, h.ID, r.RunID, b.Sequence, data, r.Status == "exited")
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var agent, harness string
			var ended bool
			if err = tx.QueryRow(ctx, `SELECT record->>'agent',record->>'harness',ended FROM hrx_hub_runs WHERE host_id=$1 AND run_id=$2`, h.ID, r.RunID).Scan(&agent, &harness, &ended); err != nil {
				return err
			}
			if agent != r.Agent || harness != r.Harness || !ended {
				return ErrConflict
			}
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO hrx_hub_receipts(host_id,sequence,payload_digest) VALUES($1,$2,$3)`, h.ID, b.Sequence, digest); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE hrx_hub_hosts SET last_sequence=$2 WHERE host_id=$1`, h.ID, b.Sequence); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Acquire(ctx context.Context, h Host, r LeaseRequest) (Lease, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback(ctx)
	h, err = lockHost(ctx, tx, h)
	if err != nil {
		return Lease{}, err
	}
	if err = r.validate(h); err != nil {
		return Lease{}, err
	}
	if r.Generation != 0 {
		return Lease{}, ErrInvalid
	}
	var l Lease
	var priorIdentity string
	var priorGeneration int64
	err = tx.QueryRow(ctx, `SELECT identity,generation FROM hrx_hub_claims WHERE host_id=$1 AND run_id=$2`, h.ID, r.RunID).Scan(&priorIdentity, &priorGeneration)
	if err == nil {
		if priorIdentity != r.Identity {
			return Lease{}, ErrConflict
		}
		err = tx.QueryRow(ctx, `SELECT identity,host_id,run_id,generation,expires_at,clock_timestamp() FROM hrx_hub_leases WHERE identity=$1 AND host_id=$2 AND run_id=$3 AND generation=$4 AND expires_at>clock_timestamp()`, r.Identity, h.ID, r.RunID, priorGeneration).Scan(&l.Identity, &l.HostID, &l.RunID, &l.Generation, &l.ExpiresAt, &l.ServerTime)
		if errors.Is(err, pgx.ErrNoRows) {
			return Lease{}, ErrConflict
		}
		return l, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, err
	}
	var ended bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hrx_hub_runs WHERE host_id=$1 AND run_id=$2 AND ended)`, h.ID, r.RunID).Scan(&ended); err != nil {
		return Lease{}, err
	}
	if ended {
		return Lease{}, ErrConflict
	}
	err = tx.QueryRow(ctx, `INSERT INTO hrx_hub_leases(identity,host_id,run_id,generation,expires_at) VALUES($1,$2,$3,1,clock_timestamp()+$4*interval '1 second')
ON CONFLICT(identity) DO UPDATE SET host_id=EXCLUDED.host_id,run_id=EXCLUDED.run_id,
generation=CASE WHEN hrx_hub_leases.host_id=EXCLUDED.host_id AND hrx_hub_leases.run_id=EXCLUDED.run_id AND hrx_hub_leases.expires_at>clock_timestamp() THEN hrx_hub_leases.generation ELSE hrx_hub_leases.generation+1 END,
expires_at=EXCLUDED.expires_at
WHERE hrx_hub_leases.expires_at<=clock_timestamp() OR (hrx_hub_leases.host_id=EXCLUDED.host_id AND hrx_hub_leases.run_id=EXCLUDED.run_id)
RETURNING identity,host_id,run_id,generation,expires_at,clock_timestamp()`, r.Identity, h.ID, r.RunID, s.LeaseTTL.Seconds()).Scan(&l.Identity, &l.HostID, &l.RunID, &l.Generation, &l.ExpiresAt, &l.ServerTime)
	if errors.Is(err, pgx.ErrNoRows) {
		var owner OwnershipConflict
		if err := tx.QueryRow(ctx, `SELECT host_id,run_id,generation,expires_at FROM hrx_hub_leases WHERE identity=$1`, r.Identity).Scan(&owner.HostID, &owner.RunID, &owner.Generation, &owner.ExpiresAt); err != nil {
			return Lease{}, err
		}
		return Lease{}, &owner
	}
	if err != nil {
		return Lease{}, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO hrx_hub_claims(host_id,run_id,identity,generation) VALUES($1,$2,$3,$4)`, h.ID, r.RunID, r.Identity, l.Generation); err != nil {
		return Lease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Lease{}, err
	}
	return l, nil
}

func (s *Store) Renew(ctx context.Context, h Host, r LeaseRequest) (Lease, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback(ctx)
	h, err = lockHost(ctx, tx, h)
	if err != nil {
		return Lease{}, err
	}
	if err = r.validate(h); err != nil {
		return Lease{}, err
	}
	if r.Generation < 1 {
		return Lease{}, ErrInvalid
	}
	var l Lease
	err = tx.QueryRow(ctx, `UPDATE hrx_hub_leases SET expires_at=clock_timestamp()+$5*interval '1 second' WHERE identity=$1 AND host_id=$2 AND run_id=$3 AND generation=$4 AND expires_at>clock_timestamp() RETURNING identity,host_id,run_id,generation,expires_at,clock_timestamp()`, r.Identity, h.ID, r.RunID, r.Generation, s.LeaseTTL.Seconds()).Scan(&l.Identity, &l.HostID, &l.RunID, &l.Generation, &l.ExpiresAt, &l.ServerTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrConflict
	}
	if err != nil {
		return Lease{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Lease{}, err
	}
	return l, nil
}

func (s *Store) Release(ctx context.Context, h Host, r LeaseRequest) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	h, err = lockHost(ctx, tx, h)
	if err != nil {
		return err
	}
	if err = r.validate(h); err != nil {
		return err
	}
	if r.Generation < 1 {
		return ErrInvalid
	}
	tag, err := tx.Exec(ctx, `UPDATE hrx_hub_leases SET expires_at=LEAST(expires_at,clock_timestamp()) WHERE identity=$1 AND host_id=$2 AND run_id=$3 AND generation=$4`, r.Identity, h.ID, r.RunID, r.Generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}

func (s *Store) Snapshot(ctx context.Context) (monitor.Snapshot, error) {
	result := monitor.Snapshot{Version: Version, GeneratedAt: time.Now().UTC(), Agents: []monitor.Row{}, Sources: []monitor.Source{}}
	rows, err := s.Pool.Query(ctx, `SELECT r.record,r.ended,h.revoked,(r.live_seen_at>clock_timestamp()-$1*interval '1 second') IS TRUE,(h.last_heartbeat>clock_timestamp()-$1*interval '1 second') IS TRUE,COALESCE(c.generation,0),l.expires_at,COALESCE(l.host_id=r.host_id AND l.run_id=r.run_id,false),COALESCE(l.expires_at>clock_timestamp(),false) FROM hrx_hub_runs r JOIN hrx_hub_hosts h USING(host_id) LEFT JOIN hrx_hub_leases l ON l.identity=r.record->>'agent' LEFT JOIN hrx_hub_claims c ON c.host_id=r.host_id AND c.run_id=r.run_id WHERE NOT r.archived ORDER BY r.host_id,r.run_id LIMIT 5001`, s.StaleAfter.Seconds())
	if err != nil {
		return result, err
	}
	defer rows.Close()
	metadataBytes := 0
	for rows.Next() {
		var data []byte
		var ended, revoked, live, hostLive, owner, leaseLive bool
		var generation int64
		var expires *time.Time
		if err = rows.Scan(&data, &ended, &revoked, &live, &hostLive, &generation, &expires, &owner, &leaseLive); err != nil {
			return result, err
		}
		metadataBytes += len(data)
		if len(result.Agents) >= 5000 || metadataBytes > 3*1024*1024 {
			return monitor.Snapshot{}, ErrCapacity
		}
		var r monitor.Record
		if json.Unmarshal(data, &r) != nil {
			return result, fmt.Errorf("invalid stored hub observation")
		}
		sourceStatus := "connected"
		if !hostLive || revoked {
			sourceStatus = "stale"
		}
		if ended {
			r.Status = "exited"
		} else if !live || !hostLive || revoked {
			r.Status = "stale"
		}
		row := monitor.Row{Record: r, Source: r.HostID, SourceStatus: sourceStatus}
		if generation > 0 && expires != nil {
			state := "inactive"
			if leaseLive && owner && !revoked {
				state = "held"
			} else if !owner {
				state = "superseded"
			}
			row.Ownership = &monitor.Ownership{State: state, Generation: generation}
			if owner {
				row.Ownership.ExpiresAt = expires
			}
		}
		result.Agents = append(result.Agents, row)
	}
	if err = rows.Err(); err != nil {
		return result, err
	}
	hosts, err := s.Pool.Query(ctx, `SELECT host_id,last_heartbeat,revoked,(last_heartbeat>clock_timestamp()-$1*interval '1 second') IS TRUE FROM hrx_hub_hosts ORDER BY host_id LIMIT 1001`, s.StaleAfter.Seconds())
	if err != nil {
		return result, err
	}
	defer hosts.Close()
	for hosts.Next() {
		if len(result.Sources) >= 1000 {
			return monitor.Snapshot{}, ErrCapacity
		}
		var source monitor.Source
		var last *time.Time
		var revoked, live bool
		if err = hosts.Scan(&source.ID, &last, &revoked, &live); err != nil {
			return result, err
		}
		source.Status = "stale"
		if live && !revoked {
			source.Status = "connected"
		}
		if last != nil {
			source.LastSuccess = *last
		}
		result.Sources = append(result.Sources, source)
	}
	return result, hosts.Err()
}
