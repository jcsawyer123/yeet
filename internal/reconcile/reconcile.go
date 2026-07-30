// Package reconcile keeps yeet's local database in sync with Coolify's
// live state. This is Phase 1 of the reaper design: reconciliation only -
// it adopts yeet-tagged resources, tracks their observed status, and
// detects drift (resources deleted outside of yeet). It never takes a
// destructive action; enforcement (TTL/idle/reset) is a later phase built
// on top of this once reconciliation has proven itself reliable.
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
}

func New(client *coolify.Client, db *store.DB, selfUUID string) *Reconciler {
	return &Reconciler{client: client, db: db, selfUUID: selfUUID}
}

// Run ticks every interval until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := r.tick(); err != nil {
			log.Printf("reconcile: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Reconciler) tick() error {
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
