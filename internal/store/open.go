package store

import "context"

// Open returns the product store. An empty dsn uses the in-memory
// implementation (tests and local binaries). A Postgres DSN (Cloud SQL via
// Auth Proxy at 127.0.0.1, or any pgx URL) opens PostgreSQL and applies
// schema. The closer must be called when the process exits.
func Open(ctx context.Context, dsn string) (Store, ControllerStore, func() error, error) {
	return openStore(ctx, dsn, true)
}

// OpenWithoutMigrate is for ignition-controller: DML only, no schema owner.
func OpenWithoutMigrate(ctx context.Context, dsn string) (Store, ControllerStore, func() error, error) {
	return openStore(ctx, dsn, false)
}

func openStore(ctx context.Context, dsn string, migrate bool) (Store, ControllerStore, func() error, error) {
	if dsn == "" {
		m := NewMemory()
		return m, m, func() error { return nil }, nil
	}
	var (
		p   *Postgres
		err error
	)
	if migrate {
		p, err = OpenPostgres(ctx, dsn)
	} else {
		p, err = OpenPostgresNoMigrate(ctx, dsn)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	return p, p, p.Close, nil
}

// DevSeeder is implemented by Memory and Postgres for IGNITION_DEV_BEARER.
type DevSeeder interface {
	SeedRole(projectID, subject, role string)
	SeedImage(projectID, imageID string)
}
