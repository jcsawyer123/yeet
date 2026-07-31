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

	if err := db.CreateInstance(p.ID, "wake-test", "uuid-1", "application", nil, nil); err != nil {
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
