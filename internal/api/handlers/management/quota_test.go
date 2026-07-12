package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestIsTransientQuotaRefreshError(t *testing.T) {
	if !isTransientQuotaRefreshError(errors.New("Post https://oauth2.googleapis.com/token: net/http: TLS handshake timeout")) {
		t.Fatal("TLS handshake timeout should be retried")
	}
	if isTransientQuotaRefreshError(errors.New(`oauth status 400: {"error":"invalid_grant"}`)) {
		t.Fatal("invalid_grant must not be retried")
	}
	if isTransientQuotaRefreshError(context.Canceled) {
		t.Fatal("canceled request must not be retried")
	}
}

func TestResolveAntigravityProjectIDDiscoversAndStoresMissingProject(t *testing.T) {
	original := fetchAntigravityProjectID
	fetchAntigravityProjectID = func(_ context.Context, token string, _ *http.Client) (string, error) {
		if token != "fresh-access-token" {
			t.Fatalf("token = %q, want refreshed access token", token)
		}
		return "discovered-project", nil
	}
	t.Cleanup(func() { fetchAntigravityProjectID = original })

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{Provider: "antigravity", Metadata: map[string]any{"refresh_token": "refresh-token"}}
	if !h.canFetchAntigravityOfficialQuota(auth) {
		t.Fatal("refresh-token-only credential should be eligible for quota fetching")
	}
	projectID, err := h.resolveAntigravityProjectID(context.Background(), auth, "fresh-access-token")
	if err != nil {
		t.Fatalf("resolve project id: %v", err)
	}
	if projectID != "discovered-project" || authProjectID(auth) != "discovered-project" {
		t.Fatalf("resolved project = %q, auth metadata = %#v", projectID, auth.Metadata)
	}
}

func TestAntigravityFiveHourQuotaByFamilySeparatesPools(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	zero := 0.0
	half := 0.5
	weekly := 0.9
	state := antigravityQuotaFileState{
		Status: "success",
		Groups: []antigravityQuotaGroup{
			{ID: "gemini-five-hour", ResetTime: now.Add(5 * time.Hour).Format(time.RFC3339), RemainingFraction: &zero},
			{ID: "claude-gpt-five-hour", ResetTime: now.Add(5 * time.Hour).Format(time.RFC3339), RemainingFraction: &half},
			{ID: "gemini-weekly", ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339), RemainingFraction: &weekly},
		},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal quota state: %v", err)
	}

	got := antigravityFiveHourQuotaByFamily(raw, now)
	if len(got) != 2 {
		t.Fatalf("five-hour families = %#v, want gemini and claude-gpt only", got)
	}
	if got["gemini"].RemainingPercent != 0 {
		t.Fatalf("gemini remaining = %v, want 0", got["gemini"].RemainingPercent)
	}
	if got["claude-gpt"].RemainingPercent != 50 {
		t.Fatalf("claude/gpt remaining = %v, want 50", got["claude-gpt"].RemainingPercent)
	}
}

func TestCompleteAntigravityQuotaGroupsFillsMissingWindows(t *testing.T) {
	now := time.Now().UTC()
	remaining := 0.75
	existing := []antigravityAvailableModelsGroup{{
		ID: "gemini-weekly",
		Buckets: []antigravityAvailableModelsBucket{{
			ID: "gemini-weekly", Window: "weekly", RemainingFraction: &remaining,
			ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
		}},
	}}
	models := json.RawMessage(fmt.Sprintf(`{
		"gemini-short":{"quotaInfo":{"remainingFraction":0.9,"resetTime":%q}},
		"gemini-weekly":{"quotaInfo":{"remainingFraction":0.8,"resetTime":%q}},
		"claude-short":{"quotaInfo":{"remainingFraction":0.7,"resetTime":%q}},
		"claude-weekly":{"quotaInfo":{"remainingFraction":0.6,"resetTime":%q}}
	}`,
		now.Add(5*time.Hour).Format(time.RFC3339),
		now.Add(7*24*time.Hour).Format(time.RFC3339),
		now.Add(5*time.Hour).Format(time.RFC3339),
		now.Add(7*24*time.Hour).Format(time.RFC3339),
	))

	groups := completeAntigravityQuotaGroups(existing, models, now)
	seen := map[string]bool{}
	for _, group := range groups {
		for _, key := range antigravityQuotaGroupKeys(group, now) {
			seen[key] = true
		}
	}
	for _, key := range []string{"gemini-short", "gemini-weekly", "claude-gpt-short", "claude-gpt-weekly"} {
		if !seen[key] {
			t.Fatalf("missing completed quota group %q in %#v", key, groups)
		}
	}
}

func TestValidatePersistedAntigravityQuotaSuccessAcceptsPoolBuckets(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	full := 1.0
	state := antigravityQuotaFileState{
		Status: "success",
		Groups: []antigravityQuotaGroup{
			{
				DisplayName: "Gemini Models",
				Description: "Gemini Flash, Gemini Pro",
				Buckets: []antigravityQuotaBucket{
					{BucketID: "gemini-5h", DisplayName: "Five Hour Limit", Window: "5h", RemainingFraction: &full, ResetTime: now.Add(5 * time.Hour).Format(time.RFC3339)},
					{BucketID: "gemini-weekly", DisplayName: "Weekly Limit", Window: "weekly", RemainingFraction: &full, ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339)},
				},
			},
			{
				DisplayName: "Claude and GPT models",
				Description: "Claude Opus, Claude Sonnet, GPT-OSS",
				Buckets: []antigravityQuotaBucket{
					{BucketID: "3p-5h", DisplayName: "Five Hour Limit", Window: "5h", RemainingFraction: &full, ResetTime: now.Add(5 * time.Hour).Format(time.RFC3339)},
					{BucketID: "3p-weekly", DisplayName: "Weekly Limit", Window: "weekly", RemainingFraction: &full, ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339)},
				},
			},
		},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal quota state: %v", err)
	}
	if err := validatePersistedAntigravityQuotaSuccess(raw, now); err != nil {
		t.Fatalf("two pool groups with four buckets should be complete: %v", err)
	}
}

func TestCompleteAntigravityQuotaGroupsFillsOmittedInactiveShortWindows(t *testing.T) {
	now := time.Now().UTC()
	remaining := 0.75
	groups := []antigravityAvailableModelsGroup{
		{
			ID: "gemini-weekly",
			Buckets: []antigravityAvailableModelsBucket{{
				ID: "gemini-weekly", Window: "weekly", RemainingFraction: &remaining,
				ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
			}},
		},
		{
			ID: "claude-gpt-weekly",
			Buckets: []antigravityAvailableModelsBucket{{
				ID: "claude-gpt-weekly", Window: "weekly", RemainingFraction: &remaining,
				ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
			}},
		},
	}

	completed := completeAntigravityQuotaGroups(groups, nil, now)
	seen := map[string]antigravityAvailableModelsBucket{}
	for _, group := range completed {
		keys := antigravityQuotaGroupKeys(group, now)
		if len(keys) > 0 && len(group.Buckets) > 0 {
			seen[keys[0]] = group.Buckets[0]
		}
	}
	for _, key := range []string{"gemini-short", "claude-gpt-short"} {
		bucket, ok := seen[key]
		if !ok || bucket.RemainingFraction == nil || *bucket.RemainingFraction != 1 {
			t.Fatalf("inferred bucket %q = %+v, want fully available", key, bucket)
		}
		if bucket.ResetTime != "" {
			t.Fatalf("inferred bucket %q reset time = %q, want empty for inactive window", key, bucket.ResetTime)
		}
	}
}

func TestCompleteAntigravityQuotaGroupsDoesNotHidePartialResponses(t *testing.T) {
	now := time.Now().UTC()
	remaining := 0.75
	groups := []antigravityAvailableModelsGroup{
		{
			ID: "gemini-weekly",
			Buckets: []antigravityAvailableModelsBucket{{
				ID: "gemini-weekly", Window: "weekly", RemainingFraction: &remaining,
			}},
		},
		{
			ID: "claude-gpt-weekly",
			Buckets: []antigravityAvailableModelsBucket{{
				ID: "claude-gpt-weekly", Window: "weekly", RemainingFraction: &remaining,
			}},
		},
		{
			ID: "gemini-short",
			Buckets: []antigravityAvailableModelsBucket{{
				ID: "gemini-short", Window: "short", RemainingFraction: &remaining,
			}},
		},
	}

	completed := completeAntigravityQuotaGroups(groups, nil, now)
	if len(completed) != len(groups) {
		t.Fatalf("partial response expanded from %d to %d groups", len(groups), len(completed))
	}
}

func TestMissingAntigravityWeeklyActivationProbesSelectsMissingFamilies(t *testing.T) {
	now := time.Now().UTC()
	remaining := 0.8
	payload := map[string]any{
		"groups": []antigravityAvailableModelsGroup{
			{
				ID: "gemini-weekly",
				Buckets: []antigravityAvailableModelsBucket{{
					ID: "gemini-weekly", Window: "weekly", RemainingFraction: &remaining,
				}},
			},
			{
				ID: "claude-gpt-weekly",
				Buckets: []antigravityAvailableModelsBucket{{
					ID: "claude-gpt-weekly", Window: "weekly", RemainingFraction: &remaining,
					ResetTime: now.Add(6 * 24 * time.Hour).Format(time.RFC3339),
				}},
			},
		},
		"models": json.RawMessage(`{
			"gemini-3-pro":{"quotaInfo":{"remainingFraction":0.8}},
			"gemini-2.5-flash-lite":{"quotaInfo":{"remainingFraction":1}},
			"claude-opus-4-6":{"quotaInfo":{"remainingFraction":0.8}},
			"claude-haiku-4-5":{"quotaInfo":{"remainingFraction":1}}
		}`),
	}

	probes := missingAntigravityWeeklyActivationProbes(payload, now)
	if len(probes) != 1 {
		t.Fatalf("probes = %#v, want one missing family", probes)
	}
	if probes[0].Family != "gemini" || probes[0].Model != "gemini-2.5-flash-lite" {
		t.Fatalf("probe = %#v, want cheapest Gemini model", probes[0])
	}
}

func TestMissingAntigravityWeeklyActivationProbesIncludesGeminiAndClaude(t *testing.T) {
	now := time.Now().UTC()
	payload := map[string]any{
		"models": json.RawMessage(`{
			"gemini-2.5-flash":{"quotaInfo":{"remainingFraction":1}},
			"claude-sonnet-4-6":{"quotaInfo":{"remainingFraction":1}},
			"claude-haiku-4-5":{"quotaInfo":{"remainingFraction":1}}
		}`),
	}

	probes := missingAntigravityWeeklyActivationProbes(payload, now)
	if len(probes) != 2 {
		t.Fatalf("probes = %#v, want Gemini and Claude/GPT", probes)
	}
	if probes[0].Family != "gemini" || probes[0].Model != "gemini-2.5-flash" {
		t.Fatalf("Gemini probe = %#v", probes[0])
	}
	if probes[1].Family != "claude-gpt" || probes[1].Model != "claude-haiku-4-5" {
		t.Fatalf("Claude probe = %#v, want Haiku preference", probes[1])
	}
}

func TestMissingAntigravityWeeklyActivationProbesTreatsFreshSevenDayWindowAsInactive(t *testing.T) {
	now := time.Now().UTC()
	remaining := 1.0
	payload := map[string]any{
		"groups": []antigravityAvailableModelsGroup{{
			ID: "gemini-weekly",
			Buckets: []antigravityAvailableModelsBucket{{
				ID: "gemini-weekly", Window: "weekly", RemainingFraction: &remaining,
				ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
			}},
		}},
		"models": json.RawMessage(`{"gemini-2.5-flash-lite":{"quotaInfo":{"remainingFraction":1}}}`),
	}

	probes := missingAntigravityWeeklyActivationProbes(payload, now)
	if len(probes) != 1 || probes[0].Family != "gemini" {
		t.Fatalf("probes = %#v, want fresh seven-day Gemini window activated", probes)
	}
}

func TestMissingAntigravityWeeklyActivationProbesFallsBackWhenModelsAreMissing(t *testing.T) {
	now := time.Now().UTC()
	remaining := 1.0
	payload := map[string]any{
		"groups": []antigravityAvailableModelsGroup{
			{
				DisplayName: "Gemini Models",
				Buckets: []antigravityAvailableModelsBucket{{
					Window: "weekly", RemainingFraction: &remaining,
					ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
				}},
			},
			{
				DisplayName: "Claude and GPT models",
				Buckets: []antigravityAvailableModelsBucket{{
					Window: "weekly", RemainingFraction: &remaining,
					ResetTime: now.Add(7 * 24 * time.Hour).Format(time.RFC3339),
				}},
			},
		},
	}

	probes := missingAntigravityWeeklyActivationProbes(payload, now)
	if len(probes) != 2 {
		t.Fatalf("probes = %#v, want both fallback families", probes)
	}
	if probes[0].Family != "gemini" || probes[0].Model != "gemini-2.5-flash-lite" {
		t.Fatalf("Gemini probe = %#v", probes[0])
	}
	if probes[1].Family != "claude-gpt" || probes[1].Model != "claude-haiku-4-5" {
		t.Fatalf("Claude/GPT probe = %#v", probes[1])
	}
}

func TestReserveAntigravityWeeklyActivationProbeCooldown(t *testing.T) {
	resetAntigravityWeeklyActivationProbeState()
	t.Cleanup(resetAntigravityWeeklyActivationProbeState)
	now := time.Now()
	if !reserveAntigravityWeeklyActivationProbe("auth-1\x00gemini", now) {
		t.Fatal("first probe should be reserved")
	}
	if reserveAntigravityWeeklyActivationProbe("auth-1\x00gemini", now.Add(time.Minute)) {
		t.Fatal("probe inside cooldown should be rejected")
	}
	if !reserveAntigravityWeeklyActivationProbe("auth-1\x00claude-gpt", now.Add(time.Minute)) {
		t.Fatal("different family should have its own reservation")
	}
	if !reserveAntigravityWeeklyActivationProbe("auth-1\x00gemini", now.Add(antigravityWeeklyActivationProbeCooldown)) {
		t.Fatal("probe at cooldown boundary should be accepted")
	}
}

func TestAntigravityAccountTierLabel(t *testing.T) {
	tests := []struct {
		name string
		tier *antigravityAccountTier
		want string
	}{
		{name: "missing", tier: nil, want: ""},
		{name: "pro id", tier: &antigravityAccountTier{ID: "g1-pro-tier"}, want: "Pro"},
		{name: "ultra name", tier: &antigravityAccountTier{Name: "Google AI Ultra"}, want: "Ultra"},
		{name: "standard id", tier: &antigravityAccountTier{ID: "standard-tier"}, want: "Free"},
		{name: "unknown fallback", tier: &antigravityAccountTier{ID: "custom", Name: "Enterprise"}, want: "Enterprise"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := antigravityAccountTierLabel(tt.tier); got != tt.want {
				t.Fatalf("antigravityAccountTierLabel(%+v) = %q, want %q", tt.tier, got, tt.want)
			}
		})
	}
}

func TestResetQuota_UsesAuthIndex(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auth := &coreauth.Auth{
		ID:             "reset-auth-id",
		FileName:       "reset-auth-file.json",
		Provider:       "claude",
		Status:         coreauth.StatusError,
		StatusMessage:  "quota exhausted",
		Unavailable:    true,
		NextRetryAfter: next,
		Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
		ModelStates: map[string]*coreauth.ModelState{
			"claude-reset-model": {
				Status:         coreauth.StatusError,
				StatusMessage:  "quota exhausted",
				Unavailable:    true,
				NextRetryAfter: next,
				Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
			},
		},
	}
	authIndex := auth.EnsureIndex()
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/reset-quota", strings.NewReader(`{"auth_index":"`+authIndex+`"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.ResetQuota(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode response: %v", errUnmarshal)
	}
	if payload["auth_index"] != authIndex {
		t.Fatalf("auth_index = %#v, want %q", payload["auth_index"], authIndex)
	}

	updated, ok := manager.GetByID("reset-auth-id")
	if !ok || updated == nil {
		t.Fatalf("expected auth record to exist after reset")
	}
	if updated.Status != coreauth.StatusActive || updated.StatusMessage != "" || updated.Unavailable || !updated.NextRetryAfter.IsZero() {
		t.Fatalf("updated auth state = status %q message %q unavailable %v next %v", updated.Status, updated.StatusMessage, updated.Unavailable, updated.NextRetryAfter)
	}
	if updated.Quota.Exceeded || updated.Quota.Reason != "" || !updated.Quota.NextRecoverAt.IsZero() || updated.Quota.BackoffLevel != 0 {
		t.Fatalf("updated auth quota = %+v, want cleared", updated.Quota)
	}
	state := updated.ModelStates["claude-reset-model"]
	if state == nil {
		t.Fatalf("expected model state to remain")
	}
	if state.Status != coreauth.StatusActive || state.StatusMessage != "" || state.Unavailable || !state.NextRetryAfter.IsZero() {
		t.Fatalf("updated model state = status %q message %q unavailable %v next %v", state.Status, state.StatusMessage, state.Unavailable, state.NextRetryAfter)
	}
	if state.Quota.Exceeded || state.Quota.Reason != "" || !state.Quota.NextRecoverAt.IsZero() || state.Quota.BackoffLevel != 0 {
		t.Fatalf("updated model quota = %+v, want cleared", state.Quota)
	}
}

func TestResetQuota_DoesNotAcceptAuthIDOrFileName(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "reset-auth-id-only",
		FileName: "reset-auth-file-only.json",
		Provider: "claude",
		Status:   coreauth.StatusError,
	}
	authIndex := auth.EnsureIndex()
	if authIndex == auth.ID || authIndex == auth.FileName {
		t.Fatalf("test auth_index unexpectedly matches id or file name: %q", authIndex)
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("failed to register auth record: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{name: "auth_id field ignored", body: `{"auth_id":"reset-auth-id-only"}`, wantCode: http.StatusBadRequest},
		{name: "id field ignored", body: `{"id":"reset-auth-id-only"}`, wantCode: http.StatusBadRequest},
		{name: "file name is not an index", body: `{"auth_index":"reset-auth-file-only.json"}`, wantCode: http.StatusNotFound},
		{name: "auth id is not an index", body: `{"auth_index":"reset-auth-id-only"}`, wantCode: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			req := httptest.NewRequest(http.MethodPost, "/v0/management/reset-quota", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			ctx.Request = req
			h.ResetQuota(ctx)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d with body %s", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

func completeAntigravityQuotaResponse(weeklyReset string) []byte {
	return []byte(fmt.Sprintf(`{"groups":[
		{"id":"gemini","displayName":"Gemini","buckets":[
			{"id":"gemini-short","window":"5-hour","remainingFraction":0.8},
			{"id":"gemini-weekly","window":"weekly","resetTime":%q,"remainingFraction":0.5}
		]},
		{"id":"claude-gpt","displayName":"Claude/GPT","buckets":[
			{"id":"claude-gpt-short","window":"5-hour","remainingFraction":0.7},
			{"id":"claude-gpt-weekly","window":"weekly","resetTime":%q,"remainingFraction":0.4}
		]}
	]}`, weeklyReset, weeklyReset))
}

func TestPersistAntigravityQuotaSnapshotFromAPICall(t *testing.T) {
	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	auth := &coreauth.Auth{
		ID:       "persist-antigravity-auth",
		FileName: "persist-antigravity-auth.json",
		Provider: "antigravity",
	}
	body := completeAntigravityQuotaResponse("2030-01-02T03:04:05Z")
	if err := h.persistAntigravityQuotaSnapshotFromAPICall(auth, "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", http.StatusOK, body); err != nil {
		t.Fatalf("persist quota snapshot: %v", err)
	}

	state, err := h.readAntigravityQuotaState()
	if err != nil {
		t.Fatalf("read quota state: %v", err)
	}
	raw, ok := state.Files[auth.FileName]
	if !ok {
		t.Fatalf("missing persisted state for %q", auth.FileName)
	}
	var persisted antigravityQuotaFileState
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("decode persisted quota state: %v", err)
	}
	if persisted.Status != "success" || len(persisted.Groups) != 2 || persisted.Groups[0].DisplayName != "Gemini" {
		t.Fatalf("persisted quota state = %+v", persisted)
	}
}

func TestPersistAntigravityQuotaSnapshotFromAvailableModels(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{
		ID:       "persist-antigravity-models",
		FileName: "persist-antigravity-models.json",
		Provider: "antigravity",
	}
	body := []byte(`{"models":{"gemini-2.5-flash":{"quotaInfo":{"remainingFraction":0.99,"resetTime":"2030-01-01T05:00:00Z"}},"claude-sonnet":{"quotaInfo":{"remainingFraction":0.32,"resetTime":"2030-01-04T05:00:00Z"}}}}`)
	if err := h.persistAntigravityQuotaSnapshotFromAPICall(auth, "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels", http.StatusOK, body); err != nil {
		t.Fatalf("persist models quota snapshot: %v", err)
	}

	state, err := h.readAntigravityQuotaState()
	if err != nil {
		t.Fatalf("read quota state: %v", err)
	}
	var persisted antigravityQuotaFileState
	if err := json.Unmarshal(state.Files[auth.FileName], &persisted); err != nil {
		t.Fatalf("decode persisted quota state: %v", err)
	}
	if persisted.Status != "success" || len(persisted.Groups) != 4 {
		t.Fatalf("persisted quota state = %+v, want weekly and inactive short groups for both families", persisted)
	}
	if len(persisted.Groups[0].Buckets) != 1 || persisted.Groups[0].Buckets[0].RemainingFraction == nil {
		t.Fatalf("first persisted quota group = %+v", persisted.Groups[0])
	}
}

func TestPersistAntigravityQuotaErrorFromAPICall(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{
		ID:       "failed-antigravity-auth",
		FileName: "failed-antigravity-auth.json",
		Provider: "antigravity",
	}
	if err := h.persistAntigravityQuotaSnapshotFromAPICall(auth, "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", http.StatusUnauthorized, []byte("{\"error\":\"token expired\"}")); err != nil {
		t.Fatalf("persist quota error: %v", err)
	}

	state, err := h.readAntigravityQuotaState()
	if err != nil {
		t.Fatalf("read quota state: %v", err)
	}
	var persisted struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(state.Files[auth.FileName], &persisted); err != nil {
		t.Fatalf("decode persisted quota error: %v", err)
	}
	if persisted.Status != "error" || !strings.Contains(persisted.Error, "401") {
		t.Fatalf("persisted quota error = %+v", persisted)
	}
}

func TestPersistAntigravityQuotaErrorPreservesLastSuccessfulGroups(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	auth := &coreauth.Auth{
		ID: "failed-after-success", FileName: "failed-after-success.json", Provider: "antigravity",
	}
	resetAt := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	body := completeAntigravityQuotaResponse(resetAt)
	if err := h.persistAntigravityQuotaSnapshotFromAPICall(auth, "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", http.StatusOK, body); err != nil {
		t.Fatalf("persist successful quota: %v", err)
	}
	before, err := h.readAntigravityQuotaState()
	if err != nil {
		t.Fatal(err)
	}
	var successful map[string]json.RawMessage
	if err := json.Unmarshal(before.Files[auth.FileName], &successful); err != nil {
		t.Fatal(err)
	}

	if err := h.persistAntigravityQuotaSnapshotFromAPICall(auth, "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", http.StatusUnauthorized, []byte(`{"error":"expired"}`)); err != nil {
		t.Fatalf("persist failed attempt: %v", err)
	}
	after, err := h.readAntigravityQuotaState()
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]json.RawMessage
	if err := json.Unmarshal(after.Files[auth.FileName], &persisted); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = json.Unmarshal(persisted["status"], &status)
	if status != "error" || len(persisted["groups"]) == 0 {
		t.Fatalf("failed attempt did not retain successful groups: %s", after.Files[auth.FileName])
	}
	if string(persisted["refreshed_at"]) != string(successful["refreshed_at"]) || len(persisted["attempted_at"]) == 0 {
		t.Fatalf("refresh timestamps were not preserved: %s", after.Files[auth.FileName])
	}
	if snapshots := antigravityWeeklyQuotaByFamily(after.Files[auth.FileName], time.Now()); len(snapshots) != 2 {
		t.Fatalf("preserved error snapshot should remain routable, got %#v", snapshots)
	}
}

func TestPutAntigravityQuotaState_MergesPartialRefreshes(t *testing.T) {
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, nil)
	put := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodPut, "/v0/management/antigravity-quota-state", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ctx.Request = req
		h.PutAntigravityQuotaState(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d with body %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	put(`{"replace":true,"files":{"first.json":{"status":"success"}}}`)
	put(`{"files":{"second.json":{"status":"success"}}}`)

	state, err := h.readAntigravityQuotaState()
	if err != nil {
		t.Fatalf("read quota state: %v", err)
	}
	if len(state.Files) != 2 || state.Files["first.json"] == nil || state.Files["second.json"] == nil {
		t.Fatalf("merged files = %#v, want both snapshots", state.Files)
	}
}

func TestAntigravityQuotaStateFiltersDeletedAuthFiles(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	active := &coreauth.Auth{
		ID:       "active-auth",
		FileName: "active.json",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), active); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	if err := h.writeAntigravityQuotaState(antigravityQuotaState{Files: map[string]json.RawMessage{
		"active.json":  json.RawMessage(`{"status":"error","error":"kept"}`),
		"deleted.json": json.RawMessage(`{"status":"error","error":"stale"}`),
	}}); err != nil {
		t.Fatalf("write quota state: %v", err)
	}

	state, err := h.readAntigravityQuotaState()
	if err != nil {
		t.Fatalf("read quota state: %v", err)
	}
	if len(state.Files) != 1 || state.Files["active.json"] == nil {
		t.Fatalf("filtered files = %#v, want only active.json", state.Files)
	}
}

func TestPutAntigravityQuotaStateDoesNotReviveDeletedAuthFiles(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	active := &coreauth.Auth{
		ID:       "active-auth",
		FileName: "active.json",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), active); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	put := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		req := httptest.NewRequest(http.MethodPut, "/v0/management/antigravity-quota-state", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ctx.Request = req
		h.PutAntigravityQuotaState(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d with body %s", rec.Code, http.StatusOK, rec.Body.String())
		}
	}

	put(`{"replace":true,"files":{"active.json":{"status":"error"},"deleted.json":{"status":"error"}}}`)
	put(`{"files":{"active.json":{"status":"error","error":"updated"}}}`)

	state, err := h.readAntigravityQuotaState()
	if err != nil {
		t.Fatalf("read quota state: %v", err)
	}
	if len(state.Files) != 1 || state.Files["active.json"] == nil || state.Files["deleted.json"] != nil {
		t.Fatalf("merged files = %#v, want only active.json", state.Files)
	}
}

func TestPersistAntigravityQuotaStateFileSkipsDeletedAuthFiles(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	active := &coreauth.Auth{
		ID:       "active-auth",
		FileName: "active.json",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), active); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	if err := h.persistAntigravityQuotaStateFile("deleted.json", json.RawMessage(`{"status":"error"}`)); err != nil {
		t.Fatalf("persist deleted auth quota state: %v", err)
	}
	if err := h.persistAntigravityQuotaStateFile("active.json", json.RawMessage(`{"status":"error"}`)); err != nil {
		t.Fatalf("persist active auth quota state: %v", err)
	}

	state, err := h.readAntigravityQuotaState()
	if err != nil {
		t.Fatalf("read quota state: %v", err)
	}
	if len(state.Files) != 1 || state.Files["active.json"] == nil || state.Files["deleted.json"] != nil {
		t.Fatalf("persisted files = %#v, want only active.json", state.Files)
	}
}

func TestNormalizePersistedAntigravityTransientRecoversInterruptedRefresh(t *testing.T) {
	withGroups := normalizePersistedAntigravityTransient(json.RawMessage(`{"status":"loading","groups":[{"id":"gemini-weekly"}]}`))
	withoutGroups := normalizePersistedAntigravityTransient(json.RawMessage(`{"status":"loading","groups":[]}`))
	for name, testCase := range map[string]struct {
		raw  json.RawMessage
		want string
	}{
		"incomplete snapshot": {raw: withGroups, want: "error"},
		"never fetched":       {raw: withoutGroups, want: "idle"},
	} {
		t.Run(name, func(t *testing.T) {
			var state struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(testCase.raw, &state); err != nil {
				t.Fatal(err)
			}
			if state.Status != testCase.want {
				t.Fatalf("status = %q, want %q", state.Status, testCase.want)
			}
		})
	}
}

func TestAntigravityWeeklyQuotaByFamilyReadsFlatFrontendGroups(t *testing.T) {
	now := time.Now().UTC()
	geminiReset := now.Add(36 * time.Hour).Format(time.RFC3339)
	claudeReset := now.Add(60 * time.Hour).Format(time.RFC3339)
	raw := json.RawMessage(fmt.Sprintf(`{
		"status":"success",
		"groups":[
			{"id":"gemini-weekly","label":"Gemini weekly","models":["Gemini 3 Pro"],"remainingFraction":0.8,"resetTime":%q},
			{"id":"claude-weekly","label":"Claude/GPT weekly","models":["Claude Sonnet","GPT-OSS"],"remainingFraction":0.6,"resetTime":%q}
		]
	}`, geminiReset, claudeReset))

	snapshots := antigravityWeeklyQuotaByFamily(raw, now)
	if len(snapshots) != 2 {
		t.Fatalf("snapshots = %#v, want both model families", snapshots)
	}
	if snapshots["gemini"].RemainingPercent != 80 || snapshots["claude-gpt"].RemainingPercent != 60 {
		t.Fatalf("snapshots = %#v, want gemini 80 and claude/gpt 60", snapshots)
	}
}

func TestResetQuotaBatch_ByAuthIndexes(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	auths := []*coreauth.Auth{
		{
			ID:             "batch-auth-a",
			FileName:       "batch-auth-a.json",
			Provider:       "antigravity",
			Status:         coreauth.StatusError,
			StatusMessage:  "quota exhausted",
			Unavailable:    true,
			NextRetryAfter: next,
			Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
		},
		{
			ID:             "batch-auth-b",
			FileName:       "batch-auth-b.json",
			Provider:       "antigravity",
			Status:         coreauth.StatusError,
			StatusMessage:  "quota exhausted",
			Unavailable:    true,
			NextRetryAfter: next,
			Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 2},
		},
	}
	indexes := make([]string, 0, len(auths))
	for _, auth := range auths {
		indexes = append(indexes, auth.EnsureIndex())
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("failed to register auth record: %v", errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/reset-quota/batch", strings.NewReader(`{"auth_indexes":["`+indexes[0]+`","`+indexes[1]+`"]}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.ResetQuotaBatch(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode response: %v", errUnmarshal)
	}
	if got := int(payload["succeeded"].(float64)); got != 2 {
		t.Fatalf("succeeded = %d, want 2", got)
	}
	for _, auth := range auths {
		updated, ok := manager.GetByID(auth.ID)
		if !ok || updated == nil {
			t.Fatalf("expected auth %s to exist after reset", auth.ID)
		}
		if updated.Status != coreauth.StatusActive || updated.Quota.Exceeded || updated.Unavailable {
			t.Fatalf("updated auth %s state = status %q quota %+v unavailable %v", auth.ID, updated.Status, updated.Quota, updated.Unavailable)
		}
	}
}

func TestResetQuotaBatch_ProviderPageSizeMaxOneHundred(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	next := time.Now().Add(time.Hour)
	for i := 0; i < 105; i++ {
		auth := &coreauth.Auth{
			ID:             fmt.Sprintf("ag-batch-%03d", i),
			FileName:       fmt.Sprintf("ag-batch-%03d.json", i),
			Provider:       "antigravity",
			Status:         coreauth.StatusError,
			StatusMessage:  "quota exhausted",
			Unavailable:    true,
			NextRetryAfter: next,
			Quota:          coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: next, BackoffLevel: 1},
		}
		auth.EnsureIndex()
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("failed to register auth record: %v", errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/reset-quota", strings.NewReader(`{"provider":"antigravity","page":1,"page_size":500}`))
	req.Header.Set("Content-Type", "application/json")
	ctx.Request = req
	h.ResetQuotaBatch(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("failed to decode response: %v", errUnmarshal)
	}
	if got := int(payload["page_size"].(float64)); got != 100 {
		t.Fatalf("page_size = %d, want 100", got)
	}
	if got := int(payload["total"].(float64)); got != 105 {
		t.Fatalf("total = %d, want 105", got)
	}
	if got := int(payload["succeeded"].(float64)); got != 100 {
		t.Fatalf("succeeded = %d, want 100", got)
	}
	if hasMore, ok := payload["has_more"].(bool); !ok || !hasMore {
		t.Fatalf("has_more = %#v, want true", payload["has_more"])
	}
}
