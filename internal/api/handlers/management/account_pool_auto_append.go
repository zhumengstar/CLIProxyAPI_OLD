package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type accountPoolAutoAppendCandidate struct {
	Name      string
	Data      []byte
	Hash      string
	Quota     float64
	CheckedAt int64
}

func (h *Handler) startAccountPoolAutoAppend() {
	if h == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(accountPoolAutoAppendInterval)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), accountPoolAutoAppendInterval)
			added, skipped, err := h.appendHighQuotaAccountPoolEntries(ctx)
			cancel()
			if err != nil {
				log.WithError(err).Warn("account pool high-quota auto append failed")
				continue
			}
			if added > 0 {
				log.Infof("account pool high-quota auto append completed: added=%d skipped=%d", added, skipped)
			}
		}
	}()
}

func (h *Handler) appendHighQuotaAccountPoolEntries(ctx context.Context) (int, int, error) {
	if h == nil || h.cfg == nil || strings.TrimSpace(h.cfg.AuthDir) == "" || h.authManager == nil {
		return 0, 0, nil
	}
	candidates, err := h.highQuotaAccountPoolCandidates()
	if err != nil || len(candidates) == 0 {
		return 0, 0, err
	}
	existingHashes := h.existingAuthContentHashes()
	existingIdentities := h.existingAuthAccountIdentities()
	usedNames := h.existingAuthFileNames()
	added := 0
	skipped := 0
	for _, candidate := range candidates {
		select {
		case <-ctx.Done():
			return added, skipped, ctx.Err()
		default:
		}
		if candidate.Hash != "" {
			if _, ok := existingHashes[candidate.Hash]; ok {
				skipped++
				continue
			}
		}
		candidateIdentity := accountPoolAuthIdentityFromData(candidate.Data)
		if candidateIdentity != "" {
			if _, ok := existingIdentities[candidateIdentity]; ok {
				skipped++
				continue
			}
		}
		targetName := filepath.Base(candidate.Name)
		if targetName == "." || targetName == "" {
			targetName = "account.json"
		}
		targetName = uniqueAccountPoolAuthFileNameWithExisting(targetName, candidate.Name, usedNames)
		if errWrite := h.writeAuthFileWithArchive(ctx, targetName, candidate.Data, false); errWrite != nil {
			return added, skipped, fmt.Errorf("failed to append high-quota account %s: %w", candidate.Name, errWrite)
		}
		if candidate.Hash != "" {
			existingHashes[candidate.Hash] = struct{}{}
		}
		if candidateIdentity != "" {
			existingIdentities[candidateIdentity] = struct{}{}
		}
		usedNames[strings.ToLower(targetName)] = 1
		added++
	}
	return added, skipped, nil
}

func (h *Handler) highQuotaAccountPoolCandidates() ([]accountPoolAutoAppendCandidate, error) {
	if h == nil || h.cfg == nil {
		return nil, nil
	}
	if accountPoolPGEnabled() {
		return h.highQuotaAccountPoolCandidatesPostgres(context.Background())
	}
	accountPoolDBMu.Lock()
	defer accountPoolDBMu.Unlock()
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()
	rows, err := db.Query(`SELECT name, data, content_hash, COALESCE(check_result, ''), COALESCE(check_content_hash, '') FROM account_pool_entries`)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query account pool high quota candidates: %w", err)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool high quota rows")
		}
	}()
	candidates := make([]accountPoolAutoAppendCandidate, 0)
	for rows.Next() {
		var name, data, contentHash, rawCheck, checkContentHash string
		if errScan := rows.Scan(&name, &data, &contentHash, &rawCheck, &checkContentHash); errScan != nil {
			return nil, fmt.Errorf("failed to scan account pool high quota candidate: %w", errScan)
		}
		name = normalizeAccountPoolEntryName(name)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		entryData := bytes.TrimSpace([]byte(data))
		if len(entryData) == 0 {
			continue
		}
		rawContentHash := strings.TrimSpace(contentHash)
		if rawContentHash == "" {
			rawContentHash = hashAccountPoolContent(entryData)
		}
		if checkContentHash = strings.TrimSpace(checkContentHash); checkContentHash != "" && rawContentHash != "" && checkContentHash != rawContentHash {
			continue
		}
		quota, checkedAt, ok := highQuotaRemainingFromCheck(rawCheck)
		if !ok || quota <= accountPoolHighQuotaRemainingPercent {
			continue
		}
		authData := stripAccountPoolStateForAuthFile(entryData)
		candidates = append(candidates, accountPoolAutoAppendCandidate{Name: name, Data: authData, Hash: hashAccountPoolContent(authData), Quota: quota, CheckedAt: checkedAt})
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("failed to read account pool high quota candidates: %w", errRows)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Quota != candidates[j].Quota {
			return candidates[i].Quota > candidates[j].Quota
		}
		if candidates[i].CheckedAt != candidates[j].CheckedAt {
			return candidates[i].CheckedAt > candidates[j].CheckedAt
		}
		return strings.ToLower(candidates[i].Name) < strings.ToLower(candidates[j].Name)
	})
	return candidates, nil
}

func highQuotaRemainingFromCheck(raw string) (float64, int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, 0, false
	}
	var result accountPoolCheckResultPayload
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result.QuotaRemainingPercent == nil {
		return 0, 0, false
	}
	status := strings.ToLower(strings.TrimSpace(result.Status))
	if status != "" && status != "ok" && status != "success" && status != "healthy" {
		return 0, 0, false
	}
	if !result.RealRequestOK {
		return 0, 0, false
	}
	return *result.QuotaRemainingPercent, result.CheckedAt, true
}

func (h *Handler) existingAuthContentHashes() map[string]struct{} {
	out := make(map[string]struct{})
	if h == nil || h.cfg == nil || strings.TrimSpace(h.cfg.AuthDir) == "" {
		return out
	}
	entries, err := os.ReadDir(h.cfg.AuthDir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry == nil || entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		data, errRead := os.ReadFile(filepath.Join(h.cfg.AuthDir, entry.Name()))
		if errRead != nil || len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		out[hashAccountPoolContent(data)] = struct{}{}
	}
	return out
}

func (h *Handler) existingAuthAccountIdentities() map[string]struct{} {
	out := make(map[string]struct{})
	if h == nil {
		return out
	}
	if h.cfg != nil && strings.TrimSpace(h.cfg.AuthDir) != "" {
		entries, err := os.ReadDir(h.cfg.AuthDir)
		if err == nil {
			for _, entry := range entries {
				if entry == nil || entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
					continue
				}
				data, errRead := os.ReadFile(filepath.Join(h.cfg.AuthDir, entry.Name()))
				if errRead != nil || len(bytes.TrimSpace(data)) == 0 {
					continue
				}
				if identity := accountPoolAuthIdentityFromData(data); identity != "" {
					out[identity] = struct{}{}
				}
			}
		}
	}
	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil {
				continue
			}
			if identity := accountPoolAuthIdentityFromAuth(auth); identity != "" {
				out[identity] = struct{}{}
			}
		}
	}
	return out
}

func accountPoolAuthIdentityFromData(data []byte) string {
	for _, path := range []string{"account_id", "metadata.account_id", "metadata.chatgpt_account_id", "attributes.account_id", "attributes.chatgpt_account_id"} {
		if value := strings.TrimSpace(gjson.GetBytes(data, path).String()); value != "" {
			return "account:" + strings.ToLower(value)
		}
	}
	for _, path := range []string{"email", "metadata.email", "attributes.email"} {
		if value := strings.TrimSpace(gjson.GetBytes(data, path).String()); value != "" {
			return "email:" + strings.ToLower(value)
		}
	}
	return ""
}

func accountPoolAuthIdentityFromAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	for _, key := range []string{"account_id", "chatgpt_account_id"} {
		if auth.Attributes != nil {
			if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
				return "account:" + strings.ToLower(value)
			}
		}
		if auth.Metadata != nil {
			if value := strings.TrimSpace(fmt.Sprint(auth.Metadata[key])); value != "" && value != "<nil>" {
				return "account:" + strings.ToLower(value)
			}
		}
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["email"]); value != "" {
			return "email:" + strings.ToLower(value)
		}
	}
	if auth.Metadata != nil {
		if value := strings.TrimSpace(fmt.Sprint(auth.Metadata["email"])); value != "" && value != "<nil>" {
			return "email:" + strings.ToLower(value)
		}
	}
	return ""
}

func (h *Handler) existingAuthFileNames() map[string]int {
	out := make(map[string]int)
	if h == nil || h.cfg == nil || strings.TrimSpace(h.cfg.AuthDir) == "" {
		return out
	}
	entries, err := os.ReadDir(h.cfg.AuthDir)
	if err == nil {
		for _, entry := range entries {
			if entry != nil && !entry.IsDir() {
				out[strings.ToLower(entry.Name())] = 1
			}
		}
	}
	if h.authManager != nil {
		for _, auth := range h.authManager.List() {
			if auth == nil {
				continue
			}
			if name := strings.TrimSpace(auth.FileName); name != "" {
				out[strings.ToLower(filepath.Base(name))] = 1
			}
		}
	}
	return out
}

func uniqueAccountPoolAuthFileNameWithExisting(baseName, sourceName string, used map[string]int) string {
	baseName = filepath.Base(strings.TrimSpace(baseName))
	if baseName == "." || baseName == "" {
		baseName = "account.json"
	}
	if used == nil {
		return baseName
	}
	key := strings.ToLower(baseName)
	if used[key] == 0 {
		used[key] = 1
		return baseName
	}
	ext := filepath.Ext(baseName)
	stem := strings.TrimSuffix(baseName, ext)
	prefix := sanitizeAccountPoolAuthFileStem(sourceName)
	if prefix != "" && !strings.EqualFold(prefix, stem) {
		stem = prefix + "_" + stem
	}
	for i := used[key] + 1; i < 100000; i++ {
		nextName := fmt.Sprintf("%s_%d%s", stem, i, ext)
		lower := strings.ToLower(nextName)
		if used[lower] == 0 {
			used[key] = i
			used[lower] = 1
			return nextName
		}
	}
	return fmt.Sprintf("%s_%d%s", stem, time.Now().UnixNano(), ext)
}
