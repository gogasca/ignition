package store

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Postgres implements Store and ControllerStore against Cloud SQL PostgreSQL.
type Postgres struct {
	pool *pgxpool.Pool
}

func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	return openPostgres(ctx, dsn, true)
}

// OpenPostgresWithoutSchema connects without applying DDL (controller identity).
func OpenPostgresWithoutSchema(ctx context.Context, dsn string) (*Postgres, error) {
	return openPostgres(ctx, dsn, false)
}

func openPostgres(ctx context.Context, dsn string, initializeSchema bool) (*Postgres, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres dsn: %w", err)
	}
	cfg.MaxConns = 8
	cfg.MinConns = 1
	cfg.MaxConnIdleTime = 5 * time.Minute

	deadline := time.Now().Add(45 * time.Second)
	var pool *pgxpool.Pool
	for {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = pool.Ping(pingCtx)
			cancel()
			if err == nil {
				break
			}
			pool.Close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("postgres connect: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}

	p := &Postgres{pool: pool}
	if initializeSchema {
		if err := p.applySchema(ctx); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return p, nil
}

func (p *Postgres) Close() error {
	if p == nil || p.pool == nil {
		return nil
	}
	p.pool.Close()
	return nil
}

func (p *Postgres) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

func (p *Postgres) applySchema(ctx context.Context) error {
	for _, stmt := range splitSQL(schemaSQL) {
		if _, err := p.pool.Exec(ctx, stmt); err != nil {
			head := stmt
			if len(head) > 80 {
				head = head[:80]
			}
			return fmt.Errorf("apply schema %q: %w", head, err)
		}
	}
	return nil
}

func splitSQL(script string) []string {
	var out []string
	var b strings.Builder
	for _, line := range strings.Split(script, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
		if strings.HasSuffix(trim, ";") {
			stmt := strings.TrimSpace(b.String())
			b.Reset()
			if stmt != "" {
				out = append(out, strings.TrimSuffix(stmt, ";"))
			}
		}
	}
	if rest := strings.TrimSpace(b.String()); rest != "" {
		out = append(out, rest)
	}
	return out
}

func (p *Postgres) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	var err error
	for i := 0; i < 5; i++ {
		tx, beginErr := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
		if beginErr != nil {
			return beginErr
		}
		err = fn(tx)
		if err != nil {
			_ = tx.Rollback(ctx)
			if retryable(err) {
				time.Sleep(time.Duration(i+1) * 20 * time.Millisecond)
				continue
			}
			return err
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			err = commitErr
			if retryable(commitErr) {
				time.Sleep(time.Duration(i+1) * 20 * time.Millisecond)
				continue
			}
			return commitErr
		}
		return nil
	}
	return err
}

func retryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrIdempotencyReused
	}
	return err
}

func (p *Postgres) Role(ctx context.Context, projectID, subject string) (string, bool, error) {
	var role string
	err := p.pool.QueryRow(ctx, `SELECT role FROM role_bindings WHERE project_id=$1 AND subject=$2`, projectID, subject).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return role, true, nil
}

func (p *Postgres) SeedRole(projectID, subject, role string) {
	ctx := context.Background()
	_, _ = p.pool.Exec(ctx, `INSERT INTO projects (id, name) VALUES ($1, $1) ON CONFLICT (id) DO NOTHING`, projectID)
	_, _ = p.pool.Exec(ctx, `
		INSERT INTO role_bindings (project_id, subject, role) VALUES ($1, $2, $3)
		ON CONFLICT (project_id, subject) DO UPDATE SET role = EXCLUDED.role`, projectID, subject, role)
}

func (p *Postgres) SeedImage(projectID, imageID string) {
	ctx := context.Background()
	_, _ = p.pool.Exec(ctx, `INSERT INTO projects (id, name) VALUES ($1, $1) ON CONFLICT (id) DO NOTHING`, projectID)
	_, _ = p.pool.Exec(ctx, `
		INSERT INTO images (project_id, image_id, state) VALUES ($1, $2, 'READY')
		ON CONFLICT (project_id, image_id) DO UPDATE SET state = 'READY'`, projectID, imageID)
}

func (p *Postgres) SetSandboxState(projectID, sandboxID, state string) {
	ctx := context.Background()
	if state == "READY" {
		_, _ = p.pool.Exec(ctx, `
			UPDATE sandboxes SET state=$3, state_reason='READY', ready_time=COALESCE(ready_time, now())
			WHERE id=$2 AND project_id=$1`, projectID, sandboxID, state)
		return
	}
	_, _ = p.pool.Exec(ctx, `UPDATE sandboxes SET state=$3 WHERE id=$2 AND project_id=$1`, projectID, sandboxID, state)
}

func (p *Postgres) ResetLeaseForTest(ctx context.Context) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM controller_leases WHERE id=1`)
	return err
}

func (p *Postgres) QuotaActive(projectID string) int {
	var n int
	err := p.pool.QueryRow(context.Background(), `SELECT active FROM project_quota WHERE project_id=$1`, projectID).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

func lockIdem(ctx context.Context, tx pgx.Tx, principal, project, method, route, key string) error {
	slot := idemSlot(principal, project, method, route, key)
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, slot)
	return err
}

func lookupIdemTx(ctx context.Context, tx pgx.Tx, principal, project, method, route, key, hash string) (*IdempotencyReplay, error) {
	var recHash string
	var status int
	var body []byte
	var done bool
	err := tx.QueryRow(ctx, `
		SELECT hash, status, body, done FROM idempotency_keys
		WHERE principal=$1 AND project_id=$2 AND method=$3 AND route=$4 AND key=$5`,
		principal, project, method, route, key,
	).Scan(&recHash, &status, &body, &done)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if recHash != hash {
		return nil, ErrIdempotencyReused
	}
	if !done {
		return nil, ErrIdempotencyInProgress
	}
	return &IdempotencyReplay{Status: status, Body: body}, nil
}

func insertIdemInProgress(ctx context.Context, tx pgx.Tx, principal, project, method, route, key, hash string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (principal, project_id, method, route, key, hash, done)
		VALUES ($1, $2, $3, $4, $5, $6, FALSE)`,
		principal, project, method, route, key, hash)
	return err
}

func finishIdemTx(ctx context.Context, tx pgx.Tx, principal, project, method, route, key, hash string, status int, body []byte) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO idempotency_keys (principal, project_id, method, route, key, hash, status, body, done)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, TRUE)
		ON CONFLICT (principal, project_id, method, route, key)
		DO UPDATE SET hash=$6, status=$7, body=$8, done=TRUE`,
		principal, project, method, route, key, hash, status, body)
	return err
}

func jsonMap(m map[string]string) []byte {
	if m == nil {
		return []byte("{}")
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func jsonSlice(s []string) []byte {
	if s == nil {
		return []byte("[]")
	}
	b, err := json.Marshal(s)
	if err != nil {
		return []byte("[]")
	}
	return b
}

func jsonVal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func unmarshalJSON[T any](b []byte, dest *T) {
	if len(b) == 0 || string(b) == "null" {
		return
	}
	_ = json.Unmarshal(b, dest)
}

func ensureProject(ctx context.Context, tx pgx.Tx, projectID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO projects (id, name) VALUES ($1, $1) ON CONFLICT (id) DO NOTHING`, projectID)
	return err
}

func countsQuota(state string) bool {
	switch state {
	case "CREATING", "SCHEDULED", "STARTED", "READY":
		return true
	default:
		return false
	}
}

func listLimit(pageSize int) int {
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 100 {
		pageSize = 100
	}
	return pageSize
}

func decodeListToken(pageToken string) (string, error) {
	if pageToken == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(pageToken)
	if err != nil {
		return "", ErrInvalidArgument
	}
	return string(raw), nil
}

func encodeListToken(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func (p *Postgres) Idempotent(ctx context.Context, in IdempotentInput, fn func() (status int, body []byte, err error)) (*IdempotencyReplay, error) {
	var replay *IdempotencyReplay
	err := p.withTx(ctx, func(tx pgx.Tx) error {
		if err := lockIdem(ctx, tx, in.Principal, in.ProjectID, in.Method, in.Route, in.Key); err != nil {
			return err
		}
		r, err := lookupIdemTx(ctx, tx, in.Principal, in.ProjectID, in.Method, in.Route, in.Key, in.Hash)
		if err != nil {
			return err
		}
		if r != nil {
			replay = r
			return nil
		}
		status, body, err := fn()
		if err != nil {
			return err
		}
		if err := finishIdemTx(ctx, tx, in.Principal, in.ProjectID, in.Method, in.Route, in.Key, in.Hash, status, body); err != nil {
			return err
		}
		replay = &IdempotencyReplay{Status: status, Body: body}
		return nil
	})
	return replay, err
}

func bumpQuota(ctx context.Context, tx pgx.Tx, projectID string, delta int) error {
	if delta == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO project_quota (project_id, active) VALUES ($1, 0) ON CONFLICT (project_id) DO NOTHING`, projectID)
	if err != nil {
		return err
	}
	if delta > 0 {
		_, err = tx.Exec(ctx, `UPDATE project_quota SET active = active + $2 WHERE project_id=$1`, projectID, delta)
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE project_quota SET active = GREATEST(active + $2, 0) WHERE project_id=$1`, projectID, delta)
	return err
}

func quotaActiveTx(ctx context.Context, tx pgx.Tx, projectID string) (int, error) {
	_, err := tx.Exec(ctx, `INSERT INTO project_quota (project_id, active) VALUES ($1, 0) ON CONFLICT (project_id) DO NOTHING`, projectID)
	if err != nil {
		return 0, err
	}
	var n int
	err = tx.QueryRow(ctx, `SELECT active FROM project_quota WHERE project_id=$1 FOR UPDATE`, projectID).Scan(&n)
	return n, err
}
