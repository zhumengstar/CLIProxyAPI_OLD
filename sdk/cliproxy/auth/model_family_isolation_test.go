package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestModelFamilyFailureIsolation(t *testing.T) {
	tests := []struct {
		name           string
		failedModel    string
		failedStatus   int
		blockedModel   string
		availableModel string
	}{
		{
			name:           "gemini rate limit does not block claude",
			failedModel:    "gemini-3-pro-preview",
			failedStatus:   429,
			blockedModel:   "gemini-2.5-flash",
			availableModel: "claude-sonnet-4-5",
		},
		{
			name:           "claude rate limit does not block gemini",
			failedModel:    "claude-sonnet-4-5",
			failedStatus:   429,
			blockedModel:   "gpt-oss-120b",
			availableModel: "gemini-3-pro-preview",
		},
		{
			name:           "gemini forbidden does not block claude",
			failedModel:    "gemini-3-pro-preview",
			failedStatus:   403,
			blockedModel:   "gemini-2.5-flash",
			availableModel: "claude-sonnet-4-5",
		},
		{
			name:           "claude forbidden does not block gemini",
			failedModel:    "claude-sonnet-4-5",
			failedStatus:   403,
			blockedModel:   "gpt-oss-120b",
			availableModel: "gemini-3-pro-preview",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewManager(nil, nil, nil)
			registered, err := manager.Register(context.Background(), &Auth{
				ID:       "shared-antigravity-auth",
				Provider: "antigravity",
				Status:   StatusActive,
			})
			if err != nil {
				t.Fatalf("Register() error = %v", err)
			}

			manager.MarkResult(context.Background(), Result{
				AuthID:   registered.ID,
				Provider: registered.Provider,
				Model:    tt.failedModel,
				Error: &Error{
					HTTPStatus: tt.failedStatus,
					Message:    "upstream failure",
				},
			})

			auth, ok := manager.GetByID(registered.ID)
			if !ok {
				t.Fatal("GetByID() did not return registered auth")
			}
			selector := &RoundRobinSelector{}
			if _, errPick := selector.Pick(context.Background(), auth.Provider, tt.blockedModel, cliproxyexecutor.Options{}, []*Auth{auth}); errPick == nil {
				t.Fatalf("Pick(%q) unexpectedly succeeded", tt.blockedModel)
			}
			if picked, errPick := selector.Pick(context.Background(), auth.Provider, tt.availableModel, cliproxyexecutor.Options{}, []*Auth{auth}); errPick != nil || picked.ID != auth.ID {
				t.Fatalf("Pick(%q) = (%v, %v), want auth %q", tt.availableModel, picked, errPick, auth.ID)
			}

			if tt.failedStatus == 429 {
				state := auth.ModelStates[modelFamilyStateKey(tt.failedModel)]
				if state == nil || time.Until(state.NextRetryAfter) < rateLimitMinimumCooldown-time.Minute {
					t.Fatalf("429 family cooldown = %#v, want at least %v", state, rateLimitMinimumCooldown)
				}
			} else {
				state := auth.ModelStates[modelFamilyStateKey(tt.failedModel)]
				if state == nil || state.Status != StatusInvalid {
					t.Fatalf("403 family state = %#v, want invalid", state)
				}
			}
		})
	}
}
