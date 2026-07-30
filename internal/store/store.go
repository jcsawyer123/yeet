package store

import (
	"fmt"
	"time"
)

type Project struct {
	ID   int64
	Slug string
	Name string
	Kind string
}

// GetOrCreateProject is idempotent by slug - safe to call every reconcile
// tick for the same adopted resource without creating duplicates.
func (db *DB) GetOrCreateProject(slug, name, kind string) (*Project, error) {
	now := time.Now().Unix()
	_, err := db.sql.Exec(`
		INSERT INTO project (slug, name, kind, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO NOTHING`,
		slug, name, kind, now, now)
	if err != nil {
		return nil, fmt.Errorf("upsert project %q: %w", slug, err)
	}

	var p Project
	err = db.sql.QueryRow(`SELECT id, slug, name, kind FROM project WHERE slug = ?`, slug).
		Scan(&p.ID, &p.Slug, &p.Name, &p.Kind)
	if err != nil {
		return nil, fmt.Errorf("fetch project %q: %w", slug, err)
	}
	return &p, nil
}

// UpsertInstance is idempotent by coolify_uuid. Returns the instance's row
// id; existing rows are left untouched (observed state is updated
// separately via UpdateInstanceObserved).
func (db *DB) UpsertInstance(projectID int64, shortID, coolifyUUID, coolifyKind string) error {
	_, err := db.sql.Exec(`
		INSERT INTO instance (project_id, short_id, coolify_uuid, coolify_kind, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(coolify_uuid) DO UPDATE SET deleted_at = NULL`,
		projectID, shortID, coolifyUUID, coolifyKind, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("upsert instance %q: %w", coolifyUUID, err)
	}
	return nil
}

func (db *DB) UpdateInstanceObserved(coolifyUUID, status, fqdn string, observedAt time.Time) error {
	_, err := db.sql.Exec(`
		UPDATE instance SET observed_state = ?, fqdn = ?, observed_at = ?
		WHERE coolify_uuid = ?`,
		status, fqdn, observedAt.Unix(), coolifyUUID)
	if err != nil {
		return fmt.Errorf("update observed state for %q: %w", coolifyUUID, err)
	}
	return nil
}

// MarkInstanceDeleted flags an instance whose Coolify resource has
// disappeared. It does not delete the row - the record is kept as history.
func (db *DB) MarkInstanceDeleted(coolifyUUID string) error {
	_, err := db.sql.Exec(`
		UPDATE instance SET deleted_at = ?
		WHERE coolify_uuid = ? AND deleted_at IS NULL`,
		time.Now().Unix(), coolifyUUID)
	if err != nil {
		return fmt.Errorf("mark instance %q deleted: %w", coolifyUUID, err)
	}
	return nil
}

// LiveCoolifyUUIDs returns the coolify_uuid of every instance not marked
// deleted - used by the reconciler to detect resources that vanished from
// Coolify without going through yeet.
func (db *DB) LiveCoolifyUUIDs() ([]string, error) {
	rows, err := db.sql.Query(`SELECT coolify_uuid FROM instance WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("list live instances: %w", err)
	}
	defer rows.Close()

	var uuids []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, fmt.Errorf("scan instance uuid: %w", err)
		}
		uuids = append(uuids, uuid)
	}
	return uuids, rows.Err()
}
