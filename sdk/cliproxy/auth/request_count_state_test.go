package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestFileRequestCountStateStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".runtime", "request-counts.json")
	store := NewFileRequestCountStateStore(path)
	want := []RequestCountRecord{{
		Email:     "account@example.com",
		Success:   12,
		Failed:    3,
		UpdatedAt: time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC),
	}}
	if err := store.Save(context.Background(), want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(got) != 1 || got[0].Email != want[0].Email || got[0].Success != 12 || got[0].Failed != 3 {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestRequestCountsFollowEmailAcrossCredentialIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "request-counts.json")
	store := NewFileRequestCountStateStore(path)
	manager := NewManager(nil, nil, nil)
	if err := manager.SetRequestCountStateStore(store); err != nil {
		t.Fatalf("SetRequestCountStateStore() error = %v", err)
	}
	manager.auths["first"] = &Auth{ID: "first", Metadata: map[string]any{"email": "Same@Example.com"}}
	manager.auths["second"] = &Auth{ID: "second", Attributes: map[string]string{"email": "same@example.com"}}

	manager.MarkResult(context.Background(), Result{AuthID: "first", Success: true})
	manager.MarkResult(context.Background(), Result{AuthID: "second", Success: true})
	manager.MarkResult(context.Background(), Result{AuthID: "first", Success: false})
	if err := manager.persistRequestCounts(context.Background()); err != nil {
		t.Fatalf("persistRequestCounts() error = %v", err)
	}

	restarted := NewManager(nil, nil, nil)
	if err := restarted.SetRequestCountStateStore(store); err != nil {
		t.Fatalf("restore SetRequestCountStateStore() error = %v", err)
	}
	restarted.auths["replacement"] = &Auth{ID: "replacement", Metadata: map[string]any{"email": "same@example.com"}}
	restarted.restoreRequestCountLocked(restarted.auths["replacement"])
	got, ok := restarted.GetByID("replacement")
	if !ok {
		t.Fatal("GetByID() did not find replacement credential")
	}
	if got.Success != 2 || got.Failed != 1 {
		t.Fatalf("restored counters = success %d, failed %d; want 2 and 1", got.Success, got.Failed)
	}

	for _, auth := range manager.List() {
		if auth.Success != 2 || auth.Failed != 1 {
			t.Fatalf("duplicate email counters = success %d, failed %d; want 2 and 1", auth.Success, auth.Failed)
		}
	}
}
