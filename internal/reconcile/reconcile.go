// Package reconcile keeps yeet's local database in sync with Coolify's
// live state, and enforces the TTL/reset policy stored on each project.
//
// Every tick runs two strictly ordered phases. Reconcile always runs
// first: it adopts yeet-tagged resources, tracks their observed status,
// and detects drift (resources deleted outside of yeet). Enforce only
// runs if reconcile just succeeded - if Coolify was unreachable this
// tick, no destructive action is taken on data that might be stale. That
// ordering is the single most important correctness property here: a
// brief Coolify outage must never cascade into mass deletion.
package reconcile

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jcsawyer123/yeet/internal/coolify"
	"github.com/jcsawyer123/yeet/internal/store"
)

type Reconciler struct {
	client   *coolify.Client
	db       *store.DB
	selfUUID string // yeet's own Coolify resource - never adopted

	// Needed only for reset-via-recreate, which has to call CreateFromSpec
	// with the same target coordinates the original deploy used.
	projectUUID     string
	serverUUID      string
	environmentName string
	githubAppUUID   string

	tickMu   sync.Mutex
	lastTick time.Time // updated after every successful reconcile - dead-man's-switch for /healthz
}

func New(client *coolify.Client, db *store.DB, selfUUID, projectUUID, serverUUID, environmentName, githubAppUUID string) *Reconciler {
	return &Reconciler{
		client:          client,
		db:              db,
		selfUUID:        selfUUID,
		projectUUID:     projectUUID,
		serverUUID:      serverUUID,
		environmentName: environmentName,
		githubAppUUID:   githubAppUUID,
	}
}

// Run ticks every interval until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.reconcile(); err != nil {
			log.Printf("reconcile: %v", err)
		} else {
			r.tickMu.Lock()
			r.lastTick = time.Now()
			r.tickMu.Unlock()
			r.enforce()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// LastTick returns when reconcile last succeeded - a dead-man's-switch.
// A goroutine panic would already crash the whole process (Go re-panics
// on an unrecovered goroutine), so what this actually catches is a hang:
// reconcile blocking forever on some external call and never returning.
func (r *Reconciler) LastTick() time.Time {
	r.tickMu.Lock()
	defer r.tickMu.Unlock()
	return r.lastTick
}

func (r *Reconciler) reconcile() error {
	apps, err := r.client.ListApplications()
	if err != nil {
		return fmt.Errorf("list applications: %w", err)
	}
	services, err := r.client.ListServices()
	if err != nil {
		return fmt.Errorf("list services: %w", err)
	}

	seen := make(map[string]bool)
	for _, a := range apps {
		if a.UUID == r.selfUUID {
			continue
		}
		if err := r.adopt(a.UUID, a.Name, a.Description, a.Status, a.FQDN, "application"); err != nil {
			log.Printf("reconcile: adopt application %s: %v", a.UUID, err)
			continue
		}
		if strings.HasPrefix(a.Description, "yeet:") {
			seen[a.UUID] = true
		}
	}
	for _, sv := range services {
		if sv.UUID == r.selfUUID {
			continue
		}
		if err := r.adopt(sv.UUID, sv.Name, sv.Description, sv.Status, "", "service"); err != nil {
			log.Printf("reconcile: adopt service %s: %v", sv.UUID, err)
			continue
		}
		if strings.HasPrefix(sv.Description, "yeet:") {
			seen[sv.UUID] = true
		}
	}

	live, err := r.db.LiveCoolifyUUIDs()
	if err != nil {
		return fmt.Errorf("list live instances: %w", err)
	}
	for _, uuid := range live {
		if seen[uuid] {
			continue
		}
		if err := r.db.MarkInstanceDeleted(uuid); err != nil {
			log.Printf("reconcile: mark %s deleted: %v", uuid, err)
		}
	}
	return nil
}

func (r *Reconciler) adopt(uuid, name, description, status, fqdn, kind string) error {
	if !strings.HasPrefix(description, "yeet:") {
		return nil
	}
	slug, shortID := parseMarker(description, name)

	project, err := r.db.GetOrCreateProject(slug, slug, "adhoc")
	if err != nil {
		return err
	}
	if err := r.db.UpsertInstance(project.ID, shortID, uuid, kind); err != nil {
		return err
	}
	return r.db.UpdateInstanceObserved(uuid, status, fqdn, time.Now())
}

// enforce evaluates every instance with a TTL, idle-timeout, or reset
// policy and acts on any that are due. It's only called from Run right
// after a successful reconcile, so observed data here is at most one tick
// stale. TTL is checked before idle so an absolute deadline always wins
// over a rolling one if both somehow fire the same tick.
func (r *Reconciler) enforce() {
	enforceable, err := r.db.ListEnforceable()
	if err != nil {
		log.Printf("reconcile: list enforceable: %v", err)
		return
	}

	now := time.Now()
	for _, e := range enforceable {
		switch {
		case e.ExpiresAt != nil && !e.ExpiresAt.After(now):
			r.enforceExpiry(e, "ttl_expired", func() error { return r.db.ClearInstanceExpiry(e.InstanceID) })
		case e.IdleExpiresAt != nil && !e.IdleExpiresAt.After(now):
			r.enforceExpiry(e, "idle_expired", func() error { return r.db.ClearIdleExpiry(e.InstanceID) })
		case e.NextResetAt != nil && !e.NextResetAt.After(now):
			r.enforceReset(e)
		}
	}
}

// enforceExpiry applies the project's expiry_action (stop or delete) and
// is shared by both TTL and idle-timeout firing - they differ only in
// which deadline column triggered them and which column gets cleared
// afterward (clearAfterStop), not in what action actually happens.
func (r *Reconciler) enforceExpiry(e store.EnforceableInstance, eventKind string, clearAfterStop func() error) {
	del := e.ExpiryAction == "delete"
	var err error
	switch {
	case e.CoolifyKind == "application" && del:
		err = r.client.DeleteApplication(e.CoolifyUUID)
	case e.CoolifyKind == "application":
		err = r.client.StopApplication(e.CoolifyUUID)
	case e.CoolifyKind == "service" && del:
		err = r.client.DeleteService(e.CoolifyUUID)
	default:
		err = r.client.StopService(e.CoolifyUUID)
	}
	if err != nil {
		log.Printf("reconcile: %s action on %s: %v", eventKind, e.CoolifyUUID, err)
		_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, "error", eventKind+": "+err.Error())
		return
	}

	if del {
		_ = r.db.MarkInstanceDeleted(e.CoolifyUUID)
		_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, eventKind, "deleted")
		return
	}
	if err := clearAfterStop(); err != nil {
		log.Printf("reconcile: clear deadline for %s: %v", e.CoolifyUUID, err)
	}
	_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, eventKind, "stopped")
}

// recreateFromSpec deletes the current Coolify resource and creates a
// fresh one from spec at the same domain - the guts of "reset", shared by
// both the reaper's scheduled trigger and the on-demand ResetNow trigger
// so manual and automatic resets behave identically. Delete-then-recreate
// rather than an in-place volume wipe: guaranteed to work with endpoints
// yeet already calls, whereas Coolify's API surface for wiping a specific
// volume in place is unverified.
func (r *Reconciler) recreateFromSpec(coolifyUUID, coolifyKind, fqdn string, spec store.ProjectSpec) (*coolify.DeployResult, error) {
	var deleteErr error
	if coolifyKind == "application" {
		deleteErr = r.client.DeleteApplication(coolifyUUID)
	} else {
		deleteErr = r.client.DeleteService(coolifyUUID)
	}
	if deleteErr != nil {
		return nil, fmt.Errorf("delete: %w", deleteErr)
	}

	result, err := r.client.CreateFromSpec(coolify.DeploySpec{
		SourceType:      spec.SourceType,
		ProjectUUID:     r.projectUUID,
		ServerUUID:      r.serverUUID,
		EnvironmentName: r.environmentName,
		GithubAppUUID:   r.githubAppUUID,
		GitRepository:   spec.GitRepository,
		GitBranch:       spec.GitBranch,
		BuildPack:       spec.BuildPack,
		Dockerfile:      spec.DockerfileBlob,
		Compose:         spec.ComposeBlob,
		PortsExposes:    spec.PortsExposes,
		Name:            spec.Name,
		Description:     "yeet: " + spec.Name,
		Domains:         fqdn,
		// The old resource holding this domain was just deleted by us,
		// above - Coolify's domain-uniqueness check can lag behind that
		// deletion by a moment and 409 otherwise (observed in testing).
		ForceDomainOverride: true,
		Envs:                coolify.ParseEnvBlob(spec.EnvsBlob),
	})
	if err != nil {
		return nil, fmt.Errorf("recreate: %w", err)
	}
	return result, nil
}

func (r *Reconciler) enforceReset(e store.EnforceableInstance) {
	result, err := r.recreateFromSpec(e.CoolifyUUID, e.CoolifyKind, e.FQDN, e.Spec)
	if err != nil {
		log.Printf("reconcile: reset for project %d: %v", e.ProjectID, err)
		_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, "error", "reset: "+err.Error())
		if !strings.HasPrefix(err.Error(), "delete:") {
			// Delete succeeded (the failure was in recreate) - the old
			// resource is already gone, so mark the instance deleted so
			// it doesn't linger as a live row pointing at nothing.
			_ = r.db.MarkInstanceDeleted(e.CoolifyUUID)
		}
		return
	}

	if err := r.db.ApplyReset(e.InstanceID, result.UUID, e.ResetIntervalSeconds); err != nil {
		log.Printf("reconcile: apply reset for instance %d: %v", e.InstanceID, err)
		return
	}
	_ = r.db.UpdateInstanceObserved(result.UUID, "", e.FQDN, time.Now())
	_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, "reset", "recreated")
}

// ResetNow performs an immediate reset for a project's current live
// instance, bypassing any configured timer - used by the admin "reset
// now" endpoint. Shares recreateFromSpec with the scheduled path, so a
// manual reset behaves exactly like a timer-triggered one; the only
// difference is nothing waited for a deadline to fire it.
func (r *Reconciler) ResetNow(slug string) error {
	project, spec, err := r.db.GetProjectBySlug(slug)
	if err != nil {
		return fmt.Errorf("get project %q: %w", slug, err)
	}
	if project == nil {
		return fmt.Errorf("project %q not found", slug)
	}
	latest, err := r.db.LatestInstance(project.ID)
	if err != nil {
		return fmt.Errorf("get latest instance for %q: %w", slug, err)
	}
	if latest == nil || latest.Deleted {
		return fmt.Errorf("project %q has no live instance to reset", slug)
	}

	result, err := r.recreateFromSpec(latest.CoolifyUUID, latest.CoolifyKind, latest.FQDN, spec)
	if err != nil {
		_ = r.db.RecordEvent(&latest.ID, project.ID, "error", "manual reset: "+err.Error())
		if !strings.HasPrefix(err.Error(), "delete:") {
			_ = r.db.MarkInstanceDeleted(latest.CoolifyUUID)
		}
		return err
	}

	if err := r.db.ApplyReset(latest.ID, result.UUID, spec.ResetIntervalSeconds); err != nil {
		return fmt.Errorf("apply reset: %w", err)
	}
	_ = r.db.UpdateInstanceObserved(result.UUID, "", latest.FQDN, time.Now())
	_ = r.db.RecordEvent(&latest.ID, project.ID, "reset", "manual")
	return nil
}

// parseMarker reads the "yeet:v2 project=<slug> instance=<short_id>"
// marker if present. Otherwise this is a legacy "yeet: <name>" resource
// (everything deployed before this format existed), adopted as its own
// single-instance ad-hoc project keyed by name.
func parseMarker(description, name string) (slug, shortID string) {
	if rest, ok := strings.CutPrefix(description, "yeet:v2 "); ok {
		var project, instance string
		for _, f := range strings.Fields(rest) {
			if v, ok := strings.CutPrefix(f, "project="); ok {
				project = v
			}
			if v, ok := strings.CutPrefix(f, "instance="); ok {
				instance = v
			}
		}
		if project != "" && instance != "" {
			return project, instance
		}
	}
	return name, name
}
