package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func (p *Postgres) ListSandboxesAll(ctx context.Context) ([]Sandbox, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+sandboxCols+` FROM sandboxes ORDER BY create_time ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

func (p *Postgres) UpdateObserved(ctx context.Context, in ObservedUpdate) error {
	return p.withTx(ctx, func(tx pgx.Tx) error {
		sb, err := p.getSandboxTx(ctx, tx, in.ProjectID, in.SandboxID)
		if err != nil {
			return err
		}
		prev := sb.State
		if prev == "FINISHED" || prev == "FAILED" {
			return nil
		}
		now := time.Now().UTC()
		sb.State = in.State
		sb.StateReason = in.Reason
		switch in.State {
		case "READY":
			if sb.ReadyTime == nil {
				sb.ReadyTime = &now
			}
		case "FAILED", "FINISHED":
			sb.FinishTime = &now
			if prev == "CREATING" || prev == "SCHEDULED" || prev == "STARTED" || prev == "READY" {
				if err := bumpQuota(ctx, tx, sb.ProjectID, -1); err != nil {
					return err
				}
			}
		}
		_, err = tx.Exec(ctx, `
			UPDATE sandboxes SET state=$2, state_reason=$3, ready_time=$4, finish_time=$5 WHERE id=$1`,
			sb.ID, sb.State, sb.StateReason, sb.ReadyTime, sb.FinishTime)
		if err != nil {
			return err
		}
		if sb.OperationID == "" {
			return nil
		}
		row := tx.QueryRow(ctx, `SELECT `+opCols+` FROM operations WHERE id=$1`, sb.OperationID)
		op, err := scanOp(row.Scan)
		if err != nil {
			if err == ErrNotFound {
				return nil
			}
			return err
		}
		switch in.State {
		case "SCHEDULED", "STARTED":
			if op.StartTime == nil {
				op.StartTime = &now
			}
			if op.State == "PENDING" {
				op.State = "RUNNING"
			}
			op.ProgressMessage = in.Reason
		case "READY":
			op.State = "SUCCEEDED"
			op.EndTime = &now
			op.ProgressMessage = in.Reason
		case "FAILED":
			op.State = "FAILED"
			op.EndTime = &now
			op.ProgressMessage = in.Reason
		case "FINISHED":
			op.State = "SUCCEEDED"
			op.EndTime = &now
			op.ProgressMessage = in.Reason
		default:
			return nil
		}
		_, err = tx.Exec(ctx, `
			UPDATE operations SET state=$2, start_time=$3, end_time=$4, progress_message=$5 WHERE id=$1`,
			op.ID, op.State, op.StartTime, op.EndTime, op.ProgressMessage)
		return err
	})
}

func (p *Postgres) ListProcessesBySandbox(ctx context.Context, projectID, sandboxID string) ([]Process, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT `+processCols+` FROM processes WHERE project_id=$1 AND sandbox_id=$2 ORDER BY id ASC`,
		projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Process
	for rows.Next() {
		item, err := scanProcess(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (p *Postgres) UpdateProcessObserved(ctx context.Context, in ProcessObserved) error {
	return p.withTx(ctx, func(tx pgx.Tx) error {
		proc, err := p.getProcessTx(ctx, tx, in.ProjectID, in.SandboxID, in.ProcessID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		proc.State = in.State
		switch in.State {
		case "RUNNING", "STARTING":
			if proc.StartTime == nil {
				proc.StartTime = &now
			}
		case "FAILED", "EXITED":
			if proc.ExitTime == nil {
				proc.ExitTime = &now
			}
		}
		if in.ExitCode != nil {
			proc.ExitCode = in.ExitCode
		}
		_, err = tx.Exec(ctx, `UPDATE processes SET state=$2, start_time=$3, exit_time=$4, exit_code=$5 WHERE id=$1`,
			proc.ID, proc.State, proc.StartTime, proc.ExitTime, proc.ExitCode)
		return err
	})
}

func (p *Postgres) HoldLease(ctx context.Context, holder string, now time.Time, ttl time.Duration) (bool, error) {
	until := now.Add(ttl)
	var got string
	err := p.pool.QueryRow(ctx, `
		INSERT INTO controller_leases (id, holder, until_time)
		VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE
		SET holder = EXCLUDED.holder, until_time = EXCLUDED.until_time
		WHERE controller_leases.holder = EXCLUDED.holder
		   OR controller_leases.until_time <= $3
		RETURNING holder`, holder, until, now).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return got == holder, nil
}
