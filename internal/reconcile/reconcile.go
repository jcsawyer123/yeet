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
			r.enforce()
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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

// enforce evaluates every instance with a TTL or reset policy and acts on
// any that are due. It's only called from Run right after a successful
// reconcile, so observed data here is at most one tick stale.
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
			r.enforceTTL(e)
		case e.NextResetAt != nil && !e.NextResetAt.After(now):
			r.enforceReset(e)
		}
	}
}

func (r *Reconciler) enforceTTL(e store.EnforceableInstance) {
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
		log.Printf("reconcile: ttl action on %s: %v", e.CoolifyUUID, err)
		_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, "error", "ttl: "+err.Error())
		return
	}

	if del {
		_ = r.db.MarkInstanceDeleted(e.CoolifyUUID)
		_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, "ttl_expired", "deleted")
		return
	}
	if err := r.db.ClearInstanceExpiry(e.InstanceID); err != nil {
		log.Printf("reconcile: clear expiry for %s: %v", e.CoolifyUUID, err)
	}
	_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, "ttl_expired", "stopped")
}

// enforceReset implements "auto-reset" as delete-then-recreate from the
// project's stored source spec, rather than an in-place volume wipe -
// this is guaranteed to work with endpoints yeet already calls, whereas
// Coolify's API surface for wiping a specific volume in place is
// unverified. The domain is reused as-is so the instance's URL survives
// the reset.
func (r *Reconciler) enforceReset(e store.EnforceableInstance) {
	var deleteErr error
	if e.CoolifyKind == "application" {
		deleteErr = r.client.DeleteApplication(e.CoolifyUUID)
	} else {
		deleteErr = r.client.DeleteService(e.CoolifyUUID)
	}
	if deleteErr != nil {
		log.Printf("reconcile: reset delete %s: %v", e.CoolifyUUID, deleteErr)
		_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, "error", "reset delete: "+deleteErr.Error())
		return
	}

	result, err := r.client.CreateFromSpec(coolify.DeploySpec{
		SourceType:      e.Spec.SourceType,
		ProjectUUID:     r.projectUUID,
		ServerUUID:      r.serverUUID,
		EnvironmentName: r.environmentName,
		GithubAppUUID:   r.githubAppUUID,
		GitRepository:   e.Spec.GitRepository,
		GitBranch:       e.Spec.GitBranch,
		BuildPack:       e.Spec.BuildPack,
		Dockerfile:      e.Spec.DockerfileBlob,
		Compose:         e.Spec.ComposeBlob,
		PortsExposes:    e.Spec.PortsExposes,
		Name:            e.Spec.Name,
		Description:     "yeet: " + e.Spec.Name,
		Domains:         e.FQDN,
	})
	if err != nil {
		log.Printf("reconcile: reset recreate for project %d: %v", e.ProjectID, err)
		_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, "error", "reset recreate: "+err.Error())
		// The old resource is already gone - mark the instance deleted so
		// it doesn't linger as a live row pointing at nothing. The next
		// manual deploy (or a future retry policy) starts fresh.
		_ = r.db.MarkInstanceDeleted(e.CoolifyUUID)
		return
	}

	if err := r.db.ApplyReset(e.InstanceID, result.UUID, *e.ResetIntervalSeconds); err != nil {
		log.Printf("reconcile: apply reset for instance %d: %v", e.InstanceID, err)
		return
	}
	_ = r.db.UpdateInstanceObserved(result.UUID, "", e.FQDN, time.Now())
	_ = r.db.RecordEvent(&e.InstanceID, e.ProjectID, "reset", "recreated")
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
