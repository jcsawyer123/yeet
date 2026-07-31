package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
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

// ProjectSpec is a source-agnostic description of what a project deploys,
// stored so the reaper can recreate an instance for a reset policy without
// yeet having to remember or re-fetch the original request.
type ProjectSpec struct {
	Name                 string
	SourceType           string // "github" | "public" | "dockerfile" | "compose"
	GitRepository        string
	GitBranch            string
	BuildPack            string
	DockerfileBlob       string
	ComposeBlob          string
	PortsExposes         string
	TTLSeconds           *int64
	ResetIntervalSeconds *int64
	IdleTimeoutSeconds   *int64 // rolling window, renewed on every wake/status hit - see RenewIdleExpiry
	ExpiryAction         string // "stop" | "delete"
	DomainPattern        string // "" means the default {id}.<first allowed base>
	EnvsBlob             string // raw .env-style text; parse with coolify.ParseEnvBlob
}

// CreateProjectWithSpec registers a new project with its source spec and
// policy. Unlike GetOrCreateProject (used for adopting untracked
// resources), this always inserts - callers know this is a fresh project.
func (db *DB) CreateProjectWithSpec(spec ProjectSpec) (*Project, error) {
	expiryAction := spec.ExpiryAction
	if expiryAction == "" {
		expiryAction = "stop"
	}
	now := time.Now().Unix()
	_, err := db.sql.Exec(`
		INSERT INTO project (
			slug, name, kind, source_type, git_repository, git_branch, build_pack,
			dockerfile_blob, compose_blob, ports_exposes, ttl_seconds,
			reset_interval_seconds, idle_timeout_seconds, expiry_action, domain_pattern, envs_blob, created_at, updated_at
		) VALUES (?, ?, 'adhoc', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		spec.Name, spec.Name, spec.SourceType, spec.GitRepository, spec.GitBranch, spec.BuildPack,
		spec.DockerfileBlob, spec.ComposeBlob, spec.PortsExposes, spec.TTLSeconds,
		spec.ResetIntervalSeconds, spec.IdleTimeoutSeconds, expiryAction, spec.DomainPattern, spec.EnvsBlob, now, now)
	if err != nil {
		return nil, fmt.Errorf("create project %q: %w", spec.Name, err)
	}

	var p Project
	err = db.sql.QueryRow(`SELECT id, slug, name, kind FROM project WHERE slug = ?`, spec.Name).
		Scan(&p.ID, &p.Slug, &p.Name, &p.Kind)
	if err != nil {
		return nil, fmt.Errorf("fetch project %q: %w", spec.Name, err)
	}
	return &p, nil
}

// CreateInstance inserts a freshly-created instance with any TTL/reset
// policy computed into concrete deadlines. Unlike UpsertInstance (used by
// the reconciler for resources it didn't create), a conflict here is a
// genuine bug - the coolify_uuid was just minted - so it's a plain INSERT.
func (db *DB) CreateInstance(projectID int64, shortID, coolifyUUID, coolifyKind string, ttlSeconds, resetIntervalSeconds, idleTimeoutSeconds *int64) error {
	now := time.Now()
	var expiresAt, nextResetAt, idleExpiresAt *int64
	if ttlSeconds != nil {
		v := now.Add(time.Duration(*ttlSeconds) * time.Second).Unix()
		expiresAt = &v
	}
	if resetIntervalSeconds != nil {
		v := now.Add(time.Duration(*resetIntervalSeconds) * time.Second).Unix()
		nextResetAt = &v
	}
	if idleTimeoutSeconds != nil {
		v := now.Add(time.Duration(*idleTimeoutSeconds) * time.Second).Unix()
		idleExpiresAt = &v
	}
	_, err := db.sql.Exec(`
		INSERT INTO instance (project_id, short_id, coolify_uuid, coolify_kind, expires_at, next_reset_at, idle_expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		projectID, shortID, coolifyUUID, coolifyKind, expiresAt, nextResetAt, idleExpiresAt, now.Unix())
	if err != nil {
		return fmt.Errorf("create instance %q: %w", coolifyUUID, err)
	}
	return nil
}

// EnforceableInstance is a live instance joined with its project's policy -
// everything the reaper needs to decide on and carry out an action.
type EnforceableInstance struct {
	InstanceID           int64
	ProjectID            int64
	CoolifyUUID          string
	CoolifyKind          string
	FQDN                 string
	ExpiresAt            *time.Time
	NextResetAt          *time.Time
	IdleExpiresAt        *time.Time
	ExpiryAction         string
	ResetIntervalSeconds *int64
	Spec                 ProjectSpec
}

// ListEnforceable returns every live instance whose project has a TTL,
// reset, or idle-timeout policy configured. Instances adopted without a
// policy (legacy resources, or projects created without one) are excluded
// at the SQL level so the reaper never has to special-case them.
func (db *DB) ListEnforceable() ([]EnforceableInstance, error) {
	rows, err := db.sql.Query(`
		SELECT i.id, i.project_id, i.coolify_uuid, i.coolify_kind, i.fqdn, i.expires_at, i.next_reset_at, i.idle_expires_at,
		       p.expiry_action, p.reset_interval_seconds, p.name, p.source_type, p.git_repository,
		       p.git_branch, p.build_pack, p.dockerfile_blob, p.compose_blob, p.ports_exposes, p.envs_blob
		FROM instance i
		JOIN project p ON p.id = i.project_id
		WHERE i.deleted_at IS NULL
		  AND (i.expires_at IS NOT NULL OR i.next_reset_at IS NOT NULL OR i.idle_expires_at IS NOT NULL)`)
	if err != nil {
		return nil, fmt.Errorf("list enforceable instances: %w", err)
	}
	defer rows.Close()

	var out []EnforceableInstance
	for rows.Next() {
		var e EnforceableInstance
		var fqdn sql.NullString
		var expiresAt, nextResetAt, idleExpiresAt, resetIntervalSeconds sql.NullInt64
		if err := rows.Scan(
			&e.InstanceID, &e.ProjectID, &e.CoolifyUUID, &e.CoolifyKind, &fqdn, &expiresAt, &nextResetAt, &idleExpiresAt,
			&e.ExpiryAction, &resetIntervalSeconds, &e.Spec.Name, &e.Spec.SourceType, &e.Spec.GitRepository,
			&e.Spec.GitBranch, &e.Spec.BuildPack, &e.Spec.DockerfileBlob, &e.Spec.ComposeBlob, &e.Spec.PortsExposes, &e.Spec.EnvsBlob,
		); err != nil {
			return nil, fmt.Errorf("scan enforceable instance: %w", err)
		}
		e.FQDN = fqdn.String
		if expiresAt.Valid {
			t := time.Unix(expiresAt.Int64, 0)
			e.ExpiresAt = &t
		}
		if nextResetAt.Valid {
			t := time.Unix(nextResetAt.Int64, 0)
			e.NextResetAt = &t
		}
		if idleExpiresAt.Valid {
			t := time.Unix(idleExpiresAt.Int64, 0)
			e.IdleExpiresAt = &t
		}
		if resetIntervalSeconds.Valid {
			e.ResetIntervalSeconds = &resetIntervalSeconds.Int64
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClearInstanceExpiry stops a TTL from re-firing every tick once it's been
// acted on (stop is a one-shot action, not recurring).
func (db *DB) ClearInstanceExpiry(instanceID int64) error {
	_, err := db.sql.Exec(`UPDATE instance SET expires_at = NULL WHERE id = ?`, instanceID)
	if err != nil {
		return fmt.Errorf("clear expiry for instance %d: %w", instanceID, err)
	}
	return nil
}

// RenewIdleExpiry pushes an instance's idle deadline forward - called on
// every wake/status hit for a project with an idle timeout configured, so
// "idle" ends up meaning "nobody's asked about this in idleTimeoutSeconds"
// rather than needing any actual traffic/usage signal.
func (db *DB) RenewIdleExpiry(coolifyUUID string, idleTimeoutSeconds int64) error {
	next := time.Now().Add(time.Duration(idleTimeoutSeconds) * time.Second).Unix()
	_, err := db.sql.Exec(`UPDATE instance SET idle_expires_at = ? WHERE coolify_uuid = ?`, next, coolifyUUID)
	if err != nil {
		return fmt.Errorf("renew idle expiry for %q: %w", coolifyUUID, err)
	}
	return nil
}

// ClearIdleExpiry stops an idle timeout from re-firing every tick once
// it's fired and been acted on (stop is one-shot until the next wake
// renews it, same reasoning as ClearInstanceExpiry for TTL).
func (db *DB) ClearIdleExpiry(instanceID int64) error {
	_, err := db.sql.Exec(`UPDATE instance SET idle_expires_at = NULL WHERE id = ?`, instanceID)
	if err != nil {
		return fmt.Errorf("clear idle expiry for instance %d: %w", instanceID, err)
	}
	return nil
}

// ApplyReset records that an instance was recreated: its coolify_uuid
// changes (the old resource was deleted and a new one made), and its next
// reset deadline moves forward. observed_state/fqdn are cleared here and
// refreshed by the next call to UpdateInstanceObserved.
func (db *DB) ApplyReset(instanceID int64, newCoolifyUUID string, resetIntervalSeconds int64) error {
	next := time.Now().Add(time.Duration(resetIntervalSeconds) * time.Second).Unix()
	_, err := db.sql.Exec(`
		UPDATE instance
		SET coolify_uuid = ?, next_reset_at = ?, observed_state = NULL, observed_at = NULL
		WHERE id = ?`,
		newCoolifyUUID, next, instanceID)
	if err != nil {
		return fmt.Errorf("apply reset for instance %d: %w", instanceID, err)
	}
	return nil
}

// RecordEvent appends to an instance's audit trail. instanceID may be nil
// for project-level events with no single instance to attach to.
func (db *DB) RecordEvent(instanceID *int64, projectID int64, kind, detail string) error {
	_, err := db.sql.Exec(`
		INSERT INTO instance_event (instance_id, project_id, kind, detail, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		instanceID, projectID, kind, detail, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	return nil
}

type InstancePolicy struct {
	ExpiresAt     *time.Time
	NextResetAt   *time.Time
	IdleExpiresAt *time.Time
}

// ListInstancePolicies returns TTL/reset/idle deadlines for every live
// instance that has one, keyed by coolify_uuid, for the UI to show a
// countdown without an N+1 query per listed item.
func (db *DB) ListInstancePolicies() (map[string]InstancePolicy, error) {
	rows, err := db.sql.Query(`
		SELECT coolify_uuid, expires_at, next_reset_at, idle_expires_at FROM instance
		WHERE deleted_at IS NULL AND (expires_at IS NOT NULL OR next_reset_at IS NOT NULL OR idle_expires_at IS NOT NULL)`)
	if err != nil {
		return nil, fmt.Errorf("list instance policies: %w", err)
	}
	defer rows.Close()

	out := make(map[string]InstancePolicy)
	for rows.Next() {
		var uuid string
		var expiresAt, nextResetAt, idleExpiresAt sql.NullInt64
		if err := rows.Scan(&uuid, &expiresAt, &nextResetAt, &idleExpiresAt); err != nil {
			return nil, fmt.Errorf("scan instance policy: %w", err)
		}
		var p InstancePolicy
		if expiresAt.Valid {
			t := time.Unix(expiresAt.Int64, 0)
			p.ExpiresAt = &t
		}
		if nextResetAt.Valid {
			t := time.Unix(nextResetAt.Int64, 0)
			p.NextResetAt = &t
		}
		if idleExpiresAt.Valid {
			t := time.Unix(idleExpiresAt.Int64, 0)
			p.IdleExpiresAt = &t
		}
		out[uuid] = p
	}
	return out, rows.Err()
}

// GetProjectBySlug fetches a project along with its source spec, needed to
// recreate an instance for the wake path if the last one was deleted.
func (db *DB) GetProjectBySlug(slug string) (*Project, ProjectSpec, error) {
	var p Project
	var spec ProjectSpec
	var ttlSeconds, resetIntervalSeconds, idleTimeoutSeconds sql.NullInt64
	err := db.sql.QueryRow(`
		SELECT id, slug, name, kind, source_type, git_repository, git_branch,
		       build_pack, dockerfile_blob, compose_blob, ports_exposes, domain_pattern, envs_blob,
		       ttl_seconds, reset_interval_seconds, idle_timeout_seconds, expiry_action
		FROM project WHERE slug = ?`, slug).
		Scan(&p.ID, &p.Slug, &p.Name, &p.Kind, &spec.SourceType, &spec.GitRepository, &spec.GitBranch,
			&spec.BuildPack, &spec.DockerfileBlob, &spec.ComposeBlob, &spec.PortsExposes, &spec.DomainPattern, &spec.EnvsBlob,
			&ttlSeconds, &resetIntervalSeconds, &idleTimeoutSeconds, &spec.ExpiryAction)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ProjectSpec{}, nil
	}
	if err != nil {
		return nil, ProjectSpec{}, fmt.Errorf("get project %q: %w", slug, err)
	}
	spec.Name = p.Name
	if ttlSeconds.Valid {
		spec.TTLSeconds = &ttlSeconds.Int64
	}
	if resetIntervalSeconds.Valid {
		spec.ResetIntervalSeconds = &resetIntervalSeconds.Int64
	}
	if idleTimeoutSeconds.Valid {
		spec.IdleTimeoutSeconds = &idleTimeoutSeconds.Int64
	}
	return &p, spec, nil
}

// InstanceRef is the minimal instance state the wake path needs to decide
// what action (if any) to take.
type InstanceRef struct {
	ID            int64
	CoolifyUUID   string
	CoolifyKind   string
	FQDN          string
	ObservedState string
	Deleted       bool
}

// LatestInstance returns the most recently created instance for a project,
// deleted or not - the wake path uses this to tell "never deployed",
// "deleted, needs recreating", and "exists, just needs starting" apart.
func (db *DB) LatestInstance(projectID int64) (*InstanceRef, error) {
	var ref InstanceRef
	var fqdn, observedState sql.NullString
	var deletedAt sql.NullInt64
	err := db.sql.QueryRow(`
		SELECT id, coolify_uuid, coolify_kind, fqdn, observed_state, deleted_at
		FROM instance WHERE project_id = ? ORDER BY created_at DESC LIMIT 1`, projectID).
		Scan(&ref.ID, &ref.CoolifyUUID, &ref.CoolifyKind, &fqdn, &observedState, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest instance for project %d: %w", projectID, err)
	}
	ref.FQDN = fqdn.String
	ref.ObservedState = observedState.String
	ref.Deleted = deletedAt.Valid
	return &ref, nil
}

// CreateTriggerToken generates a fresh opaque token for a project and
// stores only its hash - the plaintext is returned once and never
// recoverable again.
func (db *DB) CreateTriggerToken(projectID int64, label string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))

	_, err := db.sql.Exec(`
		INSERT INTO trigger_token (project_id, token_hash, label, created_at)
		VALUES (?, ?, ?, ?)`,
		projectID, hex.EncodeToString(hash[:]), label, time.Now().Unix())
	if err != nil {
		return "", fmt.Errorf("store trigger token: %w", err)
	}
	return token, nil
}

// ValidateTriggerToken checks a presented token against the project it
// claims to belong to. A nil, nil return means "invalid" (unknown
// project, wrong token, or revoked) - callers should treat that as a
// generic auth failure, not distinguish why.
func (db *DB) ValidateTriggerToken(slug, token string) (*Project, error) {
	project, _, err := db.GetProjectBySlug(slug)
	if err != nil || project == nil {
		return nil, err
	}
	hash := sha256.Sum256([]byte(token))
	var count int
	err = db.sql.QueryRow(`
		SELECT COUNT(*) FROM trigger_token
		WHERE project_id = ? AND token_hash = ? AND revoked_at IS NULL`,
		project.ID, hex.EncodeToString(hash[:])).Scan(&count)
	if err != nil {
		return nil, fmt.Errorf("validate trigger token: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	return project, nil
}
