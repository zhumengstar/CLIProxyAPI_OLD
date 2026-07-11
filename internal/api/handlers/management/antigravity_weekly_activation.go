package management

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	antigravityWeeklyActivationProbeCooldown = 30 * time.Minute
	antigravityWeeklyActivationProbeTimeout  = 20 * time.Second
	antigravityWeeklyActivationFreshWindow   = 6*24*time.Hour + 23*time.Hour
)

var antigravityWeeklyActivationProbeState = struct {
	sync.Mutex
	attempts map[string]time.Time
}{attempts: make(map[string]time.Time)}

type antigravityWeeklyActivationProbe struct {
	Family string
	Model  string
}

// activateMissingAntigravityWeeklyWindows starts an otherwise inactive weekly
// quota clock with the exact credential being refreshed, then lets the caller
// retrieve and persist the updated official quota snapshot.
func (h *Handler) activateMissingAntigravityWeeklyWindows(ctx context.Context, auth *coreauth.Auth, payload map[string]any) map[string]string {
	if h == nil || h.authManager == nil || auth == nil {
		return nil
	}
	executor, ok := h.authManager.Executor("antigravity")
	if !ok || executor == nil {
		return nil
	}

	now := time.Now()
	probes := missingAntigravityWeeklyActivationProbes(payload, now)
	results := make(map[string]string, len(probes))
	for _, probe := range probes {
		key := strings.TrimSpace(auth.ID) + "\x00" + probe.Family
		if !reserveAntigravityWeeklyActivationProbe(key, now) {
			continue
		}

		probeCtx, cancel := context.WithTimeout(ctx, antigravityWeeklyActivationProbeTimeout)
		_, err := executor.Execute(probeCtx, auth, cliproxyexecutor.Request{
			Model:   probe.Model,
			Payload: []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"."}]}],"generationConfig":{"maxOutputTokens":1,"temperature":0}}}`),
		}, cliproxyexecutor.Options{
			SourceFormat:   sdktranslator.FormatAntigravity,
			ResponseFormat: sdktranslator.FormatAntigravity,
		})
		cancel()
		if err != nil {
			results[probe.Family] = "failed: " + compactQuotaErrorBody([]byte(err.Error()))
			log.WithError(err).WithFields(log.Fields{
				"auth_id": auth.ID,
				"family":  probe.Family,
				"model":   probe.Model,
			}).Warn("failed to activate antigravity weekly quota countdown")
			continue
		}
		results[probe.Family] = "activated"
		log.WithFields(log.Fields{
			"auth_id": auth.ID,
			"family":  probe.Family,
			"model":   probe.Model,
		}).Info("activated antigravity weekly quota countdown")
	}
	if len(results) == 0 {
		return nil
	}
	return results
}

func missingAntigravityWeeklyActivationProbes(payload map[string]any, now time.Time) []antigravityWeeklyActivationProbe {
	groups, _ := payload["groups"].([]antigravityAvailableModelsGroup)
	rawModels, _ := payload["models"].(json.RawMessage)
	models := antigravityAvailableModels(rawModels)

	hasWeeklyReset := make(map[string]bool, 2)
	observedFamilies := make(map[string]bool, 2)
	for _, group := range groups {
		groupName := antigravityQuotaDisplayName(group.ID, group.Label, group.DisplayName, group.DisplayNameSnake)
		family := antigravityQuotaFamily(groupName)
		if family == "" {
			continue
		}
		observedFamilies[family] = true
		for _, bucket := range group.Buckets {
			resetAt, ok := parseAntigravityResetTime(bucketResetTime(bucket))
			bucketName := antigravityQuotaDisplayName(groupName, bucket.ID, bucket.BucketID, bucket.BucketIDSnake, bucket.Label, bucket.DisplayName, bucket.DisplayNameSnake, bucket.Window)
			if ok && resetAt.After(now) && isAntigravityWeeklyQuota(bucketName, resetAt, now) && resetAt.Sub(now) < antigravityWeeklyActivationFreshWindow {
				hasWeeklyReset[family] = true
			}
		}
	}
	for _, model := range models {
		if model.QuotaInfo == nil {
			continue
		}
		modelName := antigravityModelName(model)
		family := antigravityQuotaFamily(modelName)
		resetAt, ok := parseAntigravityResetTime(model.QuotaInfo.ResetTime)
		if family != "" && ok && resetAt.After(now) && isAntigravityWeeklyQuota(modelName, resetAt, now) && resetAt.Sub(now) < antigravityWeeklyActivationFreshWindow {
			hasWeeklyReset[family] = true
		}
	}

	candidates := make(map[string][]string, 2)
	for _, model := range models {
		modelID := firstNonEmptyPlain(model.ID, model.Model, model.Name)
		family := antigravityQuotaFamily(antigravityModelName(model))
		if modelID == "" || family == "" || strings.Contains(strings.ToLower(modelID), "image") {
			continue
		}
		candidates[family] = append(candidates[family], modelID)
	}
	// The quota summary endpoint can return grouped limits without a models map.
	// Keep activation deterministic in that response shape by using lightweight,
	// generally available Antigravity models for the missing family only.
	if observedFamilies["gemini"] && len(candidates["gemini"]) == 0 {
		candidates["gemini"] = []string{"gemini-2.5-flash-lite"}
	}
	if observedFamilies["claude-gpt"] && len(candidates["claude-gpt"]) == 0 {
		candidates["claude-gpt"] = []string{"claude-haiku-4-5"}
	}

	probes := make([]antigravityWeeklyActivationProbe, 0, 2)
	for _, family := range []string{"gemini", "claude-gpt"} {
		if hasWeeklyReset[family] || len(candidates[family]) == 0 {
			continue
		}
		sort.SliceStable(candidates[family], func(i, j int) bool {
			left := antigravityActivationModelRank(family, candidates[family][i])
			right := antigravityActivationModelRank(family, candidates[family][j])
			if left != right {
				return left < right
			}
			return candidates[family][i] < candidates[family][j]
		})
		probes = append(probes, antigravityWeeklyActivationProbe{Family: family, Model: candidates[family][0]})
	}
	return probes
}

func antigravityModelName(model antigravityAvailableModelEntry) string {
	return antigravityQuotaDisplayName(model.ID, model.Name, model.DisplayName, model.DisplayNameV1, model.Model, model.APIProvider, model.ModelProvider)
}

func antigravityActivationModelRank(family, model string) int {
	value := strings.ToLower(model)
	preferences := []string{"flash-lite", "flash", "pro"}
	if family == "claude-gpt" {
		preferences = []string{"haiku", "sonnet", "gpt-oss", "gpt", "opus"}
	}
	for index, marker := range preferences {
		if strings.Contains(value, marker) {
			return index
		}
	}
	return len(preferences)
}

func reserveAntigravityWeeklyActivationProbe(key string, now time.Time) bool {
	if key == "\x00" {
		return false
	}
	antigravityWeeklyActivationProbeState.Lock()
	defer antigravityWeeklyActivationProbeState.Unlock()
	if lastAttempt, ok := antigravityWeeklyActivationProbeState.attempts[key]; ok && now.Sub(lastAttempt) < antigravityWeeklyActivationProbeCooldown {
		return false
	}
	for existingKey, attemptedAt := range antigravityWeeklyActivationProbeState.attempts {
		if now.Sub(attemptedAt) >= 2*antigravityWeeklyActivationProbeCooldown {
			delete(antigravityWeeklyActivationProbeState.attempts, existingKey)
		}
	}
	antigravityWeeklyActivationProbeState.attempts[key] = now
	return true
}

func resetAntigravityWeeklyActivationProbeState() {
	antigravityWeeklyActivationProbeState.Lock()
	defer antigravityWeeklyActivationProbeState.Unlock()
	antigravityWeeklyActivationProbeState.attempts = make(map[string]time.Time)
}
