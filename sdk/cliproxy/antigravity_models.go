package cliproxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

const (
	antigravityModelBaseURLDaily = "https://daily-cloudcode-pa.googleapis.com"
	antigravityModelBaseURLProd  = "https://cloudcode-pa.googleapis.com"
	antigravityModelsPath        = "/v1internal:fetchAvailableModels"
)

type antigravityFetchAvailableModelsResponse struct {
	Models            map[string]antigravityFetchedModel `json:"models"`
	WebSearchModelIDs []string                           `json:"webSearchModelIds"`
}

type antigravityFetchedModel struct {
	DisplayName     string `json:"displayName"`
	MaxTokens       int    `json:"maxTokens"`
	MaxOutputTokens int    `json:"maxOutputTokens"`
}

type antigravityModelCapabilityHints struct {
	WebSearchModelIDs map[string]struct{}
}

func (s *Service) fetchAntigravityModelsForAuth(ctx context.Context, auth *coreauth.Auth, staticModels []*ModelInfo) ([]*ModelInfo, bool) {
	if auth == nil || auth.Metadata == nil {
		return nil, false
	}
	accessToken, _ := auth.Metadata["access_token"].(string)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return nil, false
	}
	payload := `{}`
	if projectID, _ := auth.Metadata["project_id"].(string); strings.TrimSpace(projectID) != "" {
		if encoded, err := json.Marshal(map[string]string{"project": strings.TrimSpace(projectID)}); err == nil {
			payload = string(encoded)
		}
	}

	client := &http.Client{}
	if transport, _, errProxy := proxyutil.BuildHTTPTransport(s.antigravityModelFetchProxyURL(auth)); errProxy == nil && transport != nil {
		client.Transport = transport
	}

	for _, baseURL := range antigravityModelBaseURLs(auth) {
		req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+antigravityModelsPath, strings.NewReader(payload))
		if errReq != nil {
			continue
		}
		req.Close = true
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("User-Agent", misc.AntigravityUserAgent())

		resp, errDo := client.Do(req)
		if errDo != nil {
			continue
		}
		body, errRead := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Debugf("antigravity model fetch: close response body: %v", errClose)
		}
		if errRead != nil {
			continue
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			continue
		}
		models := parseAntigravityFetchedModels(body, staticModels)
		if len(models) > 0 {
			return models, true
		}
	}
	return nil, false
}

func (s *Service) antigravityModelFetchProxyURL(auth *coreauth.Auth) string {
	if auth != nil {
		if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
			return proxyURL
		}
	}
	if s != nil && s.cfg != nil {
		return strings.TrimSpace(s.cfg.ProxyURL)
	}
	return ""
}

func antigravityModelBaseURLs(auth *coreauth.Auth) []string {
	if baseURL := resolveAntigravityModelBaseURL(auth); baseURL != "" {
		return []string{baseURL}
	}
	return []string{antigravityModelBaseURLDaily, antigravityModelBaseURLProd}
}

func resolveAntigravityModelBaseURL(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["base_url"]); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	if auth.Metadata != nil {
		if value, ok := auth.Metadata["base_url"].(string); ok {
			value = strings.TrimSpace(value)
			if value != "" {
				return strings.TrimRight(value, "/")
			}
		}
	}
	return ""
}

func parseAntigravityModelCapabilityHints(body []byte) antigravityModelCapabilityHints {
	var parsed antigravityFetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return antigravityModelCapabilityHints{}
	}
	webSearchModels := make(map[string]struct{}, len(parsed.WebSearchModelIDs))
	for _, modelID := range parsed.WebSearchModelIDs {
		modelID = normalizeAntigravityFetchedModelID(modelID)
		if modelID != "" {
			webSearchModels[modelID] = struct{}{}
		}
	}
	return antigravityModelCapabilityHints{WebSearchModelIDs: webSearchModels}
}

func parseAntigravityFetchedModels(body []byte, staticModels []*ModelInfo) []*ModelInfo {
	var parsed antigravityFetchAvailableModelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil || len(parsed.Models) == 0 {
		return nil
	}

	staticByID := make(map[string]*ModelInfo, len(staticModels))
	for _, model := range staticModels {
		if model != nil {
			staticByID[normalizeAntigravityFetchedModelID(model.ID)] = model
		}
	}

	ids := make([]string, 0, len(parsed.Models))
	for modelID := range parsed.Models {
		if modelID = strings.TrimSpace(modelID); modelID != "" {
			ids = append(ids, modelID)
		}
	}
	sort.Strings(ids)

	hints := parseAntigravityModelCapabilityHints(body)
	models := make([]*ModelInfo, 0, len(ids))
	for _, modelID := range ids {
		fetched := parsed.Models[modelID]
		var model ModelInfo
		if static := staticByID[normalizeAntigravityFetchedModelID(modelID)]; static != nil {
			model = *static
		} else {
			displayName := strings.TrimSpace(fetched.DisplayName)
			if displayName == "" {
				displayName = modelID
			}
			model = ModelInfo{
				ID:          modelID,
				Object:      "model",
				OwnedBy:     "antigravity",
				Type:        "antigravity",
				DisplayName: displayName,
				Name:        modelID,
				Description: displayName,
			}
		}
		if fetched.MaxTokens > 0 && model.ContextLength == 0 {
			model.ContextLength = fetched.MaxTokens
		}
		if fetched.MaxOutputTokens > 0 && model.MaxCompletionTokens == 0 {
			model.MaxCompletionTokens = fetched.MaxOutputTokens
		}
		if _, ok := hints.WebSearchModelIDs[normalizeAntigravityFetchedModelID(modelID)]; ok {
			model.SupportsWebSearch = true
		}
		models = append(models, &model)
	}
	return models
}

func applyAntigravityFetchedModelCapabilities(models []*ModelInfo, hints antigravityModelCapabilityHints) []*ModelInfo {
	if len(models) == 0 || len(hints.WebSearchModelIDs) == 0 {
		return models
	}

	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := normalizeAntigravityFetchedModelID(model.ID)
		if _, ok := hints.WebSearchModelIDs[modelID]; ok {
			model.SupportsWebSearch = true
		}
	}
	return models
}

func normalizeAntigravityFetchedModelID(modelID string) string {
	return strings.ToLower(strings.TrimSpace(modelID))
}
