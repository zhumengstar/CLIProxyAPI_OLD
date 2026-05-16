package auth

import (
	"context"
	"sync"
	"testing"
	"time"
)

type deleteTrackingStore struct {
	mu      sync.Mutex
	deleted []string
}

func (s *deleteTrackingStore) List(context.Context) ([]*Auth, error) { return nil, nil }

func (s *deleteTrackingStore) Save(context.Context, *Auth) (string, error) { return "", nil }

func (s *deleteTrackingStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *deleteTrackingStore) deletedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.deleted))
	copy(out, s.deleted)
	return out
}

func TestDeleteInvalidAuthFiles_RemovesPermanentAuthFailure(t *testing.T) {
	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	auth := &Auth{
		ID:         "bad-auth",
		Provider:   "codex",
		FileName:   "bad-auth.json",
		Attributes: map[string]string{"path": "bad-auth.json"},
		LastError:  &Error{HTTPStatus: 401, Message: "invalid token"},
	}

	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	deleted := manager.DeleteInvalidAuthFiles(context.Background(), time.Now())
	if deleted != 1 {
		t.Fatalf("DeleteInvalidAuthFiles() = %d, want 1", deleted)
	}
	if got := store.deletedIDs(); len(got) != 1 || got[0] != "bad-auth.json" {
		t.Fatalf("deleted IDs = %v, want [bad-auth.json]", got)
	}

	manager.mu.RLock()
	_, exists := manager.auths["bad-auth"]
	manager.mu.RUnlock()
	if exists {
		t.Fatalf("bad-auth still exists after cleanup")
	}
}

func TestDeleteInvalidAuthFiles_RemovesQuotaExceeded(t *testing.T) {
	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	auth := &Auth{
		ID:         "quota-auth",
		Provider:   "codex",
		FileName:   "quota-auth.json",
		Attributes: map[string]string{"path": "quota-auth.json"},
		Quota:      QuotaState{Exceeded: true},
	}

	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if deleted := manager.DeleteInvalidAuthFiles(context.Background(), time.Now()); deleted != 1 {
		t.Fatalf("DeleteInvalidAuthFiles() = %d, want 1", deleted)
	}
}

func TestDeleteInvalidAuthFiles_KeepsTransientAndRuntimeAuths(t *testing.T) {
	store := &deleteTrackingStore{}
	manager := NewManager(store, nil, nil)
	auths := []*Auth{
		{
			ID:         "transient-auth",
			Provider:   "codex",
			FileName:   "transient-auth.json",
			Attributes: map[string]string{"path": "transient-auth.json"},
			LastError:  &Error{HTTPStatus: 500, Message: "upstream error"},
		},
		{
			ID:         "runtime-auth",
			Provider:   "codex",
			FileName:   "runtime-auth.json",
			Attributes: map[string]string{"runtime_only": "true", "path": "runtime-auth.json"},
			LastError:  &Error{HTTPStatus: 401, Message: "invalid token"},
		},
	}

	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("Register(%s) error = %v", auth.ID, err)
		}
	}

	if deleted := manager.DeleteInvalidAuthFiles(context.Background(), time.Now()); deleted != 0 {
		t.Fatalf("DeleteInvalidAuthFiles() = %d, want 0", deleted)
	}
	if got := store.deletedIDs(); len(got) != 0 {
		t.Fatalf("deleted IDs = %v, want none", got)
	}
}
