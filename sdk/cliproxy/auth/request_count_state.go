package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RequestCountRecord stores cumulative request counters for one account email.
type RequestCountRecord struct {
	Email     string    `json:"email"`
	Success   int64     `json:"success"`
	Failed    int64     `json:"failed,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// RequestCountStateStore persists cumulative counters independently from credentials.
type RequestCountStateStore interface {
	Load(context.Context) ([]RequestCountRecord, error)
	Save(context.Context, []RequestCountRecord) error
}

type requestCountStateFile struct {
	Version   int                  `json:"version"`
	UpdatedAt time.Time            `json:"updated_at"`
	Accounts  []RequestCountRecord `json:"accounts"`
}

// FileRequestCountStateStore stores all counters in one server-side state file.
type FileRequestCountStateStore struct {
	mu   sync.Mutex
	path string
}

func NewFileRequestCountStateStore(path string) *FileRequestCountStateStore {
	return &FileRequestCountStateStore{path: strings.TrimSpace(path)}
}

func (s *FileRequestCountStateStore) Load(ctx context.Context) ([]RequestCountRecord, error) {
	if s == nil || s.path == "" {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read request count state: %w", err)
	}
	var state requestCountStateFile
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse request count state: %w", err)
	}
	return state.Accounts, nil
}

func (s *FileRequestCountStateStore) Save(ctx context.Context, records []RequestCountRecord) error {
	if s == nil || s.path == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state := requestCountStateFile{Version: 1, UpdatedAt: time.Now().UTC(), Accounts: records}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal request count state: %w", err)
	}
	data = append(data, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Dir(s.path)
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create request count state directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create request count state temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write request count state: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close request count state: %w", err)
	}
	if err = os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("replace request count state: %w", err)
	}
	return nil
}

func requestCountEmail(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if email, ok := auth.Metadata["email"].(string); ok {
			if email = strings.ToLower(strings.TrimSpace(email)); email != "" {
				return email
			}
		}
	}
	for _, key := range []string{"email", "account_email"} {
		if email := strings.ToLower(strings.TrimSpace(auth.Attributes[key])); email != "" {
			return email
		}
	}
	return ""
}
