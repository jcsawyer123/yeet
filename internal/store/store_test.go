package store

import (
	"os"
	"testing"
)

func TestTriggerToken(t *testing.T) {
	path := "/tmp/yeet_trigger_token_test.db"
	os.Remove(path)
	defer os.Remove(path)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p, err := db.CreateProjectWithSpec(ProjectSpec{Name: "wake-test", SourceType: "dockerfile", DockerfileBlob: "FROM alpine"})
	if err != nil {
		t.Fatal(err)
	}

	token, err := db.CreateTriggerToken(p.ID, "portfolio")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("expected a non-empty token")
	}

	valid, err := db.ValidateTriggerToken("wake-test", token)
	if err != nil {
		t.Fatal(err)
	}
	if valid == nil || valid.ID != p.ID {
		t.Fatalf("expected valid token to resolve project %d, got %+v", p.ID, valid)
	}

	invalid, err := db.ValidateTriggerToken("wake-test", "wrong-token")
	if err != nil {
		t.Fatal(err)
	}
	if invalid != nil {
		t.Fatal("expected wrong token to be rejected")
	}

	unknown, err := db.ValidateTriggerToken("no-such-project", token)
	if err != nil {
		t.Fatal(err)
	}
	if unknown != nil {
		t.Fatal("expected unknown project to be rejected")
	}

	// LatestInstance: none yet
	latest, err := db.LatestInstance(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest != nil {
		t.Fatal("expected no instance yet")
	}

	if err := db.CreateInstance(p.ID, "wake-test", "uuid-1", "application", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	latest, err = db.LatestInstance(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.CoolifyUUID != "uuid-1" || latest.Deleted {
		t.Fatalf("unexpected latest instance: %+v", latest)
	}

	if err := db.MarkInstanceDeleted("uuid-1"); err != nil {
		t.Fatal(err)
	}
	latest, err = db.LatestInstance(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || !latest.Deleted {
		t.Fatalf("expected latest instance to be marked deleted, got %+v", latest)
	}
}

func i64(v int64) *int64 { return &v }

func TestIdleTimeout(t *testing.T) {
	path := "/tmp/yeet_idle_timeout_test.db"
	os.Remove(path)
	defer os.Remove(path)

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	p, err := db.CreateProjectWithSpec(ProjectSpec{
		Name: "idle-test", SourceType: "dockerfile", DockerfileBlob: "FROM alpine",
		IdleTimeoutSeconds: i64(300),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := db.CreateInstance(p.ID, "idle-test", "uuid-idle", "application", nil, nil, i64(300)); err != nil {
		t.Fatal(err)
	}

	// Should show up in ListEnforceable purely because of idle_expires_at
	// - no TTL or reset configured.
	enforceable, err := db.ListEnforceable()
	if err != nil {
		t.Fatal(err)
	}
	if len(enforceable) != 1 || enforceable[0].IdleExpiresAt == nil {
		t.Fatalf("expected 1 enforceable instance with idle_expires_at set, got %+v", enforceable)
	}
	firstDeadline := *enforceable[0].IdleExpiresAt

	// Renewing should push the deadline forward.
	if err := db.RenewIdleExpiry("uuid-idle", 300); err != nil {
		t.Fatal(err)
	}
	enforceable, err = db.ListEnforceable()
	if err != nil {
		t.Fatal(err)
	}
	if len(enforceable) != 1 || enforceable[0].IdleExpiresAt == nil {
		t.Fatalf("expected renewed instance still enforceable, got %+v", enforceable)
	}
	if !enforceable[0].IdleExpiresAt.After(firstDeadline) && !enforceable[0].IdleExpiresAt.Equal(firstDeadline) {
		t.Fatalf("expected renewed deadline >= original, got %v vs %v", *enforceable[0].IdleExpiresAt, firstDeadline)
	}

	// Clearing (as the reaper does after acting on it) should drop it out
	// of ListEnforceable since no other policy is configured.
	if err := db.ClearIdleExpiry(enforceable[0].InstanceID); err != nil {
		t.Fatal(err)
	}
	enforceable, err = db.ListEnforceable()
	if err != nil {
		t.Fatal(err)
	}
	if len(enforceable) != 0 {
		t.Fatalf("expected no enforceable instances after clearing idle expiry, got %+v", enforceable)
	}
}
