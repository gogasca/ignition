package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ignition.dev/ignition/internal/id"
)

const processCols = `id, project_id, sandbox_id, state, command, working_directory, environment,
	pty, create_time, start_time, exit_time, exit_code, terminating_signal, created_by`

func scanProcess(scan func(dest ...any) error) (Process, error) {
	var p Process
	var command, env []byte
	err := scan(
		&p.ID, &p.ProjectID, &p.SandboxID, &p.State, &command, &p.WorkingDirectory, &env,
		&p.PTY, &p.CreateTime, &p.StartTime, &p.ExitTime, &p.ExitCode, &p.TerminatingSignal, &p.CreatedBy,
	)
	if err != nil {
		return Process{}, mapErr(err)
	}
	unmarshalJSON(command, &p.Command)
	unmarshalJSON(env, &p.Environment)
	return p, nil
}

func (p *Postgres) getProcessTx(ctx context.Context, tx pgx.Tx, projectID, sandboxID, processID string) (Process, error) {
	row := tx.QueryRow(ctx, `SELECT `+processCols+` FROM processes WHERE id=$1 AND project_id=$2 AND sandbox_id=$3`,
		processID, projectID, sandboxID)
	return scanProcess(row.Scan)
}

func (p *Postgres) CreateProcess(ctx context.Context, in CreateProcessInput) (Process, *IdempotencyReplay, error) {
	var proc Process
	var replay *IdempotencyReplay
	err := p.withTx(ctx, func(tx pgx.Tx) error {
		method, route := "POST", "/sandboxes/"+in.SandboxID+"/processes"
		if err := lockIdem(ctx, tx, in.Principal, in.ProjectID, method, route, in.IdemKey); err != nil {
			return err
		}
		r, err := lookupIdemTx(ctx, tx, in.Principal, in.ProjectID, method, route, in.IdemKey, in.IdemHash)
		if err != nil {
			return err
		}
		if r != nil {
			replay = r
			return nil
		}
		sb, err := p.getSandboxTx(ctx, tx, in.ProjectID, in.SandboxID)
		if err != nil {
			return err
		}
		if sb.State != "READY" {
			return ErrFailedPrecondition
		}
		now := time.Now().UTC()
		proc = Process{
			ID:               id.New("prc"),
			ProjectID:        in.ProjectID,
			SandboxID:        in.SandboxID,
			State:            "CREATING",
			Command:          in.Command,
			WorkingDirectory: in.WorkingDir,
			Environment:      in.Environment,
			PTY:              in.PTY,
			CreateTime:       now,
			CreatedBy:        in.Principal,
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO processes (
				id, project_id, sandbox_id, state, command, working_directory, environment,
				pty, create_time, created_by
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
			proc.ID, proc.ProjectID, proc.SandboxID, proc.State, jsonSlice(proc.Command),
			proc.WorkingDirectory, jsonMap(proc.Environment), proc.PTY, proc.CreateTime, proc.CreatedBy)
		if err != nil {
			return err
		}
		body, _ := json.Marshal(proc)
		return finishIdemTx(ctx, tx, in.Principal, in.ProjectID, method, route, in.IdemKey, in.IdemHash, 200, body)
	})
	return proc, replay, err
}

func (p *Postgres) GetProcess(ctx context.Context, projectID, sandboxID, processID string) (Process, error) {
	row := p.pool.QueryRow(ctx, `SELECT `+processCols+` FROM processes WHERE id=$1 AND project_id=$2 AND sandbox_id=$3`,
		processID, projectID, sandboxID)
	return scanProcess(row.Scan)
}

func (p *Postgres) ListProcesses(ctx context.Context, projectID, sandboxID string, pageSize int, pageToken string) ([]Process, string, error) {
	limit := listLimit(pageSize)
	after, err := decodeListToken(pageToken)
	if err != nil {
		return nil, "", err
	}
	q := `SELECT ` + processCols + ` FROM processes WHERE project_id=$1 AND sandbox_id=$2`
	args := []any{projectID, sandboxID}
	if after != "" {
		q += ` AND (create_time, id) > (SELECT create_time, id FROM processes WHERE project_id=$1 AND sandbox_id=$2 AND id=$3)`
		args = append(args, after)
	}
	q += fmt.Sprintf(` ORDER BY create_time ASC, id ASC LIMIT $%d`, len(args)+1)
	args = append(args, limit+1)
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var all []Process
	for rows.Next() {
		item, err := scanProcess(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		all = append(all, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(all) > limit {
		all = all[:limit]
		next = encodeListToken(all[len(all)-1].ID)
	}
	return all, next, nil
}

func (p *Postgres) SignalProcess(ctx context.Context, projectID, sandboxID, processID, principal, key, hash, signal string) (Process, *IdempotencyReplay, error) {
	var proc Process
	var replay *IdempotencyReplay
	err := p.withTx(ctx, func(tx pgx.Tx) error {
		method, route := "POST", "/processes/"+processID+":signal"
		if err := lockIdem(ctx, tx, principal, projectID, method, route, key); err != nil {
			return err
		}
		r, err := lookupIdemTx(ctx, tx, principal, projectID, method, route, key, hash)
		if err != nil {
			return err
		}
		if r != nil {
			replay = r
			return nil
		}
		proc, err = p.getProcessTx(ctx, tx, projectID, sandboxID, processID)
		if err != nil {
			return err
		}
		if signal == "" {
			return ErrInvalidArgument
		}
		proc.TerminatingSignal = signal
		if _, err := tx.Exec(ctx, `UPDATE processes SET terminating_signal=$2 WHERE id=$1`, proc.ID, signal); err != nil {
			return err
		}
		body, _ := json.Marshal(proc)
		return finishIdemTx(ctx, tx, principal, projectID, method, route, key, hash, 200, body)
	})
	return proc, replay, err
}

func (p *Postgres) CancelProcess(ctx context.Context, projectID, sandboxID, processID, principal, key, hash string) (Process, *IdempotencyReplay, error) {
	var proc Process
	var replay *IdempotencyReplay
	err := p.withTx(ctx, func(tx pgx.Tx) error {
		method, route := "POST", "/processes/"+processID+":cancel"
		if err := lockIdem(ctx, tx, principal, projectID, method, route, key); err != nil {
			return err
		}
		r, err := lookupIdemTx(ctx, tx, principal, projectID, method, route, key, hash)
		if err != nil {
			return err
		}
		if r != nil {
			replay = r
			return nil
		}
		proc, err = p.getProcessTx(ctx, tx, projectID, sandboxID, processID)
		if err != nil {
			return err
		}
		if proc.State != "EXITED" && proc.State != "FAILED" {
			proc.State = "CANCELLING"
			if _, err := tx.Exec(ctx, `UPDATE processes SET state=$2 WHERE id=$1`, proc.ID, proc.State); err != nil {
				return err
			}
		}
		body, _ := json.Marshal(proc)
		return finishIdemTx(ctx, tx, principal, projectID, method, route, key, hash, 200, body)
	})
	return proc, replay, err
}
