package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

const imageCols = `project_id, image_id, state, state_reason, source_ref, digest, registry_ref,
	entrypoint, cmd, streaming_eligible, ineligible_reason, compressed_bytes, launch_count, create_time`

// scanImageRow scans imageCols without mapping the error, so callers can
// apply their own error semantics (a duplicate row means something different
// on insert than on lookup).
func scanImageRow(scan func(dest ...any) error) (Image, error) {
	var img Image
	var entrypoint, cmd []byte
	err := scan(
		&img.ProjectID, &img.ImageID, &img.State, &img.StateReason, &img.SourceRef, &img.Digest, &img.RegistryRef,
		&entrypoint, &cmd, &img.StreamingEligible, &img.IneligibleReason, &img.CompressedBytes, &img.LaunchCount, &img.CreateTime,
	)
	if err != nil {
		return Image{}, err
	}
	unmarshalJSON(entrypoint, &img.Entrypoint)
	unmarshalJSON(cmd, &img.Cmd)
	return img, nil
}

// CreateImage registers an already-resolved catalog row. imageId is
// immutable once admitted: a primary-key collision on (project_id, image_id)
// maps to ErrImageAlreadyExists rather than silently overwriting the pinned
// digest.
func (p *Postgres) CreateImage(ctx context.Context, in CreateImageInput) (Image, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Image{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := ensureProject(ctx, tx, in.ProjectID); err != nil {
		return Image{}, err
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO images (project_id, image_id, state, source_ref, digest, registry_ref, entrypoint, cmd, streaming_eligible, ineligible_reason, compressed_bytes)
		VALUES ($1, $2, 'READY', $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+imageCols,
		in.ProjectID, in.ImageID, in.SourceRef, in.Digest, in.RegistryRef, jsonSlice(in.Entrypoint), jsonSlice(in.Cmd),
		in.StreamingEligible, in.IneligibleReason, in.CompressedBytes,
	)
	img, err := scanImageRow(row.Scan)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Image{}, ErrImageAlreadyExists
		}
		return Image{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Image{}, err
	}
	return img, nil
}

func (p *Postgres) GetImage(ctx context.Context, projectID, imageID string) (Image, error) {
	row := p.pool.QueryRow(ctx, `SELECT `+imageCols+` FROM images WHERE project_id=$1 AND image_id=$2`, projectID, imageID)
	img, err := scanImageRow(row.Scan)
	if err != nil {
		return Image{}, mapErr(err)
	}
	return img, nil
}

// TopImagesByLaunchCount returns a project's images ordered by measured
// launch demand, most-launched first. This is the input a secondary
// boot-disk cache-epoch build selects from (see
// docs/design/ignition-design-images-startup.md — "cache large, shared,
// immutable OCI layers selected by measured launch demand"); nothing in
// Ignition reads this back for scheduling yet, so it is operator tooling,
// not part of the Store/ControllerStore interfaces.
func (p *Postgres) TopImagesByLaunchCount(ctx context.Context, projectID string, limit int) ([]Image, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := p.pool.Query(ctx,
		`SELECT `+imageCols+` FROM images WHERE project_id=$1 ORDER BY launch_count DESC, image_id ASC LIMIT $2`,
		projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Image
	for rows.Next() {
		img, err := scanImageRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}
