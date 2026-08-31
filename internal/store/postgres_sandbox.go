package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"ignition.dev/ignition/internal/id"
)

const sandboxCols = `id, project_id, name, state, state_reason, image_id, operation_id, generation,
	create_time, ready_time, finish_time, created_by, command, working_dir,
	resources, placement, timeouts, network, labels, secret_refs`

func scanSandbox(scan func(dest ...any) error) (Sandbox, error) {
	var sb Sandbox
	var command, resources, placement, timeouts, network, labels, secretRefs []byte
	err := scan(
		&sb.ID, &sb.ProjectID, &sb.Name, &sb.State, &sb.StateReason, &sb.ImageID, &sb.OperationID, &sb.Generation,
		&sb.CreateTime, &sb.ReadyTime, &sb.FinishTime, &sb.CreatedBy, &command, &sb.WorkingDir,
		&resources, &placement, &timeouts, &network, &labels, &secretRefs,
	)
	if err != nil {
		return Sandbox{}, mapErr(err)
	}
	unmarshalJSON(command, &sb.Command)
	unmarshalJSON(resources, &sb.Resources)
	unmarshalJSON(placement, &sb.Placement)
	unmarshalJSON(timeouts, &sb.Timeouts)
	unmarshalJSON(network, &sb.Network)
	unmarshalJSON(labels, &sb.Labels)
	unmarshalJSON(secretRefs, &sb.SecretRefs)
	return sb, nil
}

func (p *Postgres) getSandboxTx(ctx context.Context, tx pgx.Tx, projectID, sandboxID string) (Sandbox, error) {
	row := tx.QueryRow(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE id=$1 AND project_id=$2`, sandboxID, projectID)
	return scanSandbox(row.Scan)
}

func (p *Postgres) CreateSandbox(ctx context.Context, in CreateSandboxInput) (CreateSandboxResult, error) {
	var out CreateSandboxResult
	err := p.withTx(ctx, func(tx pgx.Tx) error {
		const method, route = "POST", "/sandboxes"
		if err := lockIdem(ctx, tx, in.Principal, in.ProjectID, method, route, in.IdemKey); err != nil {
			return err
		}
		replay, err := lookupIdemTx(ctx, tx, in.Principal, in.ProjectID, method, route, in.IdemKey, in.IdemHash)
		if err != nil {
			return err
		}
		if replay != nil {
			out = CreateSandboxResult{Replay: replay}
			return nil
		}
		if err := insertIdemInProgress(ctx, tx, in.Principal, in.ProjectID, method, route, in.IdemKey, in.IdemHash); err != nil {
			return err
		}

		if err := ensureProject(ctx, tx, in.ProjectID); err != nil {
			return err
		}
		var imgState string
		err = tx.QueryRow(ctx, `SELECT state FROM images WHERE project_id=$1 AND image_id=$2`, in.ProjectID, in.ImageID).Scan(&imgState)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrImageNotReady
			}
			return err
		}
		if imgState != "READY" {
			return ErrImageNotReady
		}

		max := in.MaxActive
		if max <= 0 {
			max = 100
		}
		active, err := quotaActiveTx(ctx, tx, in.ProjectID)
		if err != nil {
			return err
		}
		if active >= max {
			return ErrQuotaExceeded
		}

		now := time.Now().UTC()
		sbID := id.New("sbx")
		opID := id.New("op")
		sb := Sandbox{
			ID:          sbID,
			ProjectID:   in.ProjectID,
			Name:        in.Name,
			State:       "CREATING",
			StateReason: "ADMITTED",
			ImageID:     in.ImageID,
			OperationID: opID,
			Generation:  1,
			CreateTime:  now,
			CreatedBy:   in.Principal,
			Command:     in.Command,
			WorkingDir:  in.WorkingDir,
			Resources:   in.Resources,
			Placement:   in.Placement,
			Timeouts:    in.Timeouts,
			Network:     in.Network,
			Labels:      in.Labels,
			SecretRefs:  in.SecretRefs,
		}
		if sb.SecretRefs == nil {
			sb.SecretRefs = []SecretRef{}
		}
		op := Operation{
			ID:         opID,
			ProjectID:  in.ProjectID,
			Kind:       "CREATE_SANDBOX",
			State:      "PENDING",
			ResourceID: sbID,
			CreateTime: now,
			TraceID:    in.TraceID,
			CreatedBy:  in.Principal,
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO sandboxes (
				id, project_id, name, state, state_reason, image_id, operation_id, generation,
				create_time, created_by, command, working_dir,
				resources, placement, timeouts, network, labels, secret_refs
			) VALUES (
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18
			)`,
			sb.ID, sb.ProjectID, sb.Name, sb.State, sb.StateReason, sb.ImageID, sb.OperationID, sb.Generation,
			sb.CreateTime, sb.CreatedBy, jsonSlice(sb.Command), sb.WorkingDir,
			jsonVal(sb.Resources), jsonVal(sb.Placement), jsonVal(sb.Timeouts), jsonVal(sb.Network), jsonMap(sb.Labels),
			jsonVal(sb.SecretRefs),
		)
		if err != nil {
			return err
		}
		if err := insertOperation(ctx, tx, op); err != nil {
			return err
		}
		if err := bumpQuota(ctx, tx, in.ProjectID, 1); err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]any{"sandbox": sb, "operation": op})
		if err := finishIdemTx(ctx, tx, in.Principal, in.ProjectID, method, route, in.IdemKey, in.IdemHash, 202, body); err != nil {
			return err
		}
		out = CreateSandboxResult{Sandbox: sb, Operation: op}
		return nil
	})
	return out, err
}

func (p *Postgres) GetSandbox(ctx context.Context, projectID, sandboxID string) (Sandbox, error) {
	row := p.pool.QueryRow(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE id=$1 AND project_id=$2`, sandboxID, projectID)
	return scanSandbox(row.Scan)
}

func (p *Postgres) ListSandboxes(ctx context.Context, projectID string, pageSize int, pageToken string) ([]Sandbox, string, error) {
	limit := listLimit(pageSize)
	after, err := decodeListToken(pageToken)
	if err != nil {
		return nil, "", err
	}
	q := `SELECT ` + sandboxCols + ` FROM sandboxes WHERE project_id=$1`
	args := []any{projectID}
	if after != "" {
		q += ` AND (create_time, id) > (SELECT create_time, id FROM sandboxes WHERE project_id=$1 AND id=$2)`
		args = append(args, after)
	}
	q += fmt.Sprintf(` ORDER BY create_time ASC, id ASC LIMIT $%d`, len(args)+1)
	args = append(args, limit+1)
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var all []Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		all = append(all, sb)
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

func (p *Postgres) TerminateSandbox(ctx context.Context, projectID, sandboxID, principal, idemKey, idemHash, traceID string) (TerminateResult, error) {
	var out TerminateResult
	err := p.withTx(ctx, func(tx pgx.Tx) error {
		method, route := "POST", "/sandboxes/"+sandboxID+":terminate"
		if err := lockIdem(ctx, tx, principal, projectID, method, route, idemKey); err != nil {
			return err
		}
		replay, err := lookupIdemTx(ctx, tx, principal, projectID, method, route, idemKey, idemHash)
		if err != nil {
			return err
		}
		if replay != nil {
			out = TerminateResult{Replay: replay}
			return nil
		}
		sb, err := p.getSandboxTx(ctx, tx, projectID, sandboxID)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		op := Operation{
			ID:         id.New("op"),
			ProjectID:  projectID,
			Kind:       "TERMINATE_SANDBOX",
			State:      "PENDING",
			ResourceID: sb.ID,
			CreateTime: now,
			TraceID:    traceID,
			CreatedBy:  principal,
		}
		prev := sb.State
		switch prev {
		case "FINISHED", "FAILED":
			op.State = "SUCCEEDED"
			op.EndTime = &now
		case "TERMINATING":
		default:
			sb.State = "TERMINATING"
			sb.StateReason = "TERMINATE_REQUESTED"
			sb.OperationID = op.ID
			if prev == "CREATING" || prev == "SCHEDULED" || prev == "STARTED" || prev == "READY" {
				if err := bumpQuota(ctx, tx, projectID, -1); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE sandboxes SET state=$2, state_reason=$3, operation_id=$4 WHERE id=$1`,
				sb.ID, sb.State, sb.StateReason, sb.OperationID); err != nil {
				return err
			}
		}
		if err := insertOperation(ctx, tx, op); err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]any{"sandbox": sb, "operation": op})
		if err := finishIdemTx(ctx, tx, principal, projectID, method, route, idemKey, idemHash, 202, body); err != nil {
			return err
		}
		out = TerminateResult{Sandbox: sb, Operation: op}
		return nil
	})
	return out, err
}

func insertOperation(ctx context.Context, tx pgx.Tx, op Operation) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO operations (
			id, project_id, kind, state, resource_id, create_time, start_time, end_time,
			trace_id, progress_message, created_by
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		op.ID, op.ProjectID, op.Kind, op.State, op.ResourceID, op.CreateTime, op.StartTime, op.EndTime,
		op.TraceID, op.ProgressMessage, op.CreatedBy)
	return err
}

const opCols = `id, project_id, kind, state, resource_id, create_time, start_time, end_time, trace_id, progress_message, created_by`

func scanOp(scan func(dest ...any) error) (Operation, error) {
	var op Operation
	err := scan(
		&op.ID, &op.ProjectID, &op.Kind, &op.State, &op.ResourceID, &op.CreateTime,
		&op.StartTime, &op.EndTime, &op.TraceID, &op.ProgressMessage, &op.CreatedBy,
	)
	return op, mapErr(err)
}

func (p *Postgres) GetOperation(ctx context.Context, projectID, operationID string) (Operation, error) {
	row := p.pool.QueryRow(ctx, `SELECT `+opCols+` FROM operations WHERE id=$1 AND project_id=$2`, operationID, projectID)
	return scanOp(row.Scan)
}

func (p *Postgres) ListOperations(ctx context.Context, projectID string, pageSize int, pageToken, resourceID string) ([]Operation, string, error) {
	limit := listLimit(pageSize)
	after, err := decodeListToken(pageToken)
	if err != nil {
		return nil, "", err
	}
	q := `SELECT ` + opCols + ` FROM operations WHERE project_id=$1`
	args := []any{projectID}
	if resourceID != "" {
		q += fmt.Sprintf(` AND resource_id=$%d`, len(args)+1)
		args = append(args, resourceID)
	}
	if after != "" {
		q += fmt.Sprintf(` AND (create_time, id) > (SELECT create_time, id FROM operations WHERE project_id=$1 AND id=$%d)`, len(args)+1)
		args = append(args, after)
	}
	q += fmt.Sprintf(` ORDER BY create_time ASC, id ASC LIMIT $%d`, len(args)+1)
	args = append(args, limit+1)
	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var all []Operation
	for rows.Next() {
		op, err := scanOp(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		all = append(all, op)
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

func (p *Postgres) CancelOperation(ctx context.Context, projectID, operationID, principal, key, hash string) (Operation, *IdempotencyReplay, error) {
	var op Operation
	var replay *IdempotencyReplay
	err := p.withTx(ctx, func(tx pgx.Tx) error {
		method, route := "POST", "/operations/"+operationID+":cancel"
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
		row := tx.QueryRow(ctx, `SELECT `+opCols+` FROM operations WHERE id=$1 AND project_id=$2`, operationID, projectID)
		op, err = scanOp(row.Scan)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if op.State == "PENDING" || op.State == "RUNNING" {
			op.State = "CANCELLED"
			op.EndTime = &now
			if _, err := tx.Exec(ctx, `UPDATE operations SET state=$2, end_time=$3 WHERE id=$1`, op.ID, op.State, op.EndTime); err != nil {
				return err
			}
			if op.Kind == "CREATE_SANDBOX" {
				sb, err := p.getSandboxTx(ctx, tx, projectID, op.ResourceID)
				if err != nil && !errors.Is(err, ErrNotFound) {
					return err
				}
				if err == nil && countsQuota(sb.State) {
					sb.State = "FAILED"
					sb.StateReason = "CANCELLED"
					sb.FinishTime = &now
					if _, err := tx.Exec(ctx, `
						UPDATE sandboxes SET state=$2, state_reason=$3, finish_time=$4 WHERE id=$1`,
						sb.ID, sb.State, sb.StateReason, sb.FinishTime); err != nil {
						return err
					}
					if err := bumpQuota(ctx, tx, projectID, -1); err != nil {
						return err
					}
				}
			}
		}
		body, _ := json.Marshal(op)
		return finishIdemTx(ctx, tx, principal, projectID, method, route, key, hash, 200, body)
	})
	return op, replay, err
}
