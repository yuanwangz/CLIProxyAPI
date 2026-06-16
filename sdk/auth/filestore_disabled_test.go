package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type testTokenStorage struct {
	meta map[string]any
}

func (s *testTokenStorage) SetMetadata(meta map[string]any) { s.meta = meta }

func (s *testTokenStorage) SaveTokenToFile(authFilePath string) error {
	raw, err := json.Marshal(s.meta)
	if err != nil {
		return err
	}
	return os.WriteFile(authFilePath, raw, 0o600)
}

func TestFileTokenStore_Save_DisabledPersistsFlagForTokenStorage(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()
	path := filepath.Join(baseDir, "disabled.json")

	if err := os.WriteFile(path, []byte(`{"type":"test","disabled":true}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	storage := &testTokenStorage{}

	auth := &cliproxyauth.Auth{
		ID:       "disabled.json",
		Provider: "test",
		FileName: "disabled.json",
		Disabled: true,
		Storage:  storage,
		Metadata: map[string]any{"type": "test"},
	}

	if _, err := store.Save(ctx, auth); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal auth file: %v", err)
	}
	if disabled, _ := meta["disabled"].(bool); !disabled {
		t.Fatalf("disabled=%v, want true (raw=%s)", meta["disabled"], string(raw))
	}
}

func TestFileTokenStore_SaveAndList_PreservesDisabledUnauthorizedStatus(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	path := filepath.Join(baseDir, "unauthorized.json")
	if err := os.WriteFile(path, []byte(`{"type":"codex","disabled":false}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	auth := &cliproxyauth.Auth{
		ID:            "unauthorized.json",
		Provider:      "codex",
		FileName:      "unauthorized.json",
		Disabled:      true,
		Status:        cliproxyauth.StatusDisabled,
		StatusMessage: "unauthorized",
		LastError: &cliproxyauth.Error{
			Code:       "unauthorized",
			Message:    "token refresh failed with status 401: refresh_token_reused",
			HTTPStatus: http.StatusUnauthorized,
		},
		Metadata: map[string]any{"type": "codex"},
	}

	savedPath, err := store.Save(ctx, auth)
	if err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	if savedPath == "" {
		t.Fatal("Save() returned empty path")
	}

	raw, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal auth file: %v", err)
	}
	if disabled, _ := meta["disabled"].(bool); !disabled {
		t.Fatalf("disabled=%v, want true (raw=%s)", meta["disabled"], string(raw))
	}
	lastError, ok := meta["last_error"].(map[string]any)
	if !ok {
		t.Fatalf("last_error=%#v, want object (raw=%s)", meta["last_error"], string(raw))
	}
	if status, _ := lastError["http_status"].(float64); int(status) != http.StatusUnauthorized {
		t.Fatalf("last_error.http_status=%#v, want %d (raw=%s)", lastError["http_status"], http.StatusUnauthorized, string(raw))
	}

	listed, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List() len=%d, want 1", len(listed))
	}
	got := listed[0]
	if !got.Disabled {
		t.Fatal("listed Disabled=false, want true")
	}
	if got.Status != cliproxyauth.StatusDisabled {
		t.Fatalf("listed Status=%q, want %q", got.Status, cliproxyauth.StatusDisabled)
	}
	if got.LastError == nil || got.LastError.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("listed LastError=%#v, want HTTP 401", got.LastError)
	}
}

func TestFileTokenStore_SaveAndList_PreservesArchivedFlag(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)
	authPath := filepath.Join(baseDir, "archived.json")
	if err := os.WriteFile(authPath, []byte(`{"type":"codex"}`), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}

	auth := &cliproxyauth.Auth{
		ID:       "archived.json",
		Provider: "codex",
		FileName: "archived.json",
		Archived: true,
		Status:   cliproxyauth.StatusArchived,
		Metadata: map[string]any{"type": "codex"},
	}

	savedPath, err := store.Save(ctx, auth)
	if err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	raw, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal auth file: %v", err)
	}
	if archived, _ := meta["archived"].(bool); !archived {
		t.Fatalf("archived=%v, want true (raw=%s)", meta["archived"], string(raw))
	}

	listed, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List() len=%d, want 1", len(listed))
	}
	got := listed[0]
	if !got.Archived {
		t.Fatal("listed Archived=false, want true")
	}
	if got.Disabled {
		t.Fatal("listed Disabled=true, want false")
	}
	if got.Status != cliproxyauth.StatusArchived {
		t.Fatalf("listed Status=%q, want %q", got.Status, cliproxyauth.StatusArchived)
	}
}

func TestFileTokenStore_SaveAndList_PreservesCredentialCreatedAtMetadata(t *testing.T) {
	ctx := context.Background()
	baseDir := t.TempDir()

	store := NewFileTokenStore()
	store.SetBaseDir(baseDir)

	createdAt := time.Date(2026, time.January, 2, 3, 4, 5, 123456789, time.UTC)
	auth := &cliproxyauth.Auth{
		ID:        "created.json",
		Provider:  "codex",
		FileName:  "created.json",
		CreatedAt: createdAt,
		Metadata:  map[string]any{"type": "codex"},
	}

	savedPath, err := store.Save(ctx, auth)
	if err != nil {
		t.Fatalf("Save() error: %v", err)
	}
	raw, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("read auth file: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatalf("unmarshal auth file: %v", err)
	}
	if got := meta["credential_created_at"]; got != createdAt.Format(time.RFC3339Nano) {
		t.Fatalf("credential_created_at=%#v, want %q", got, createdAt.Format(time.RFC3339Nano))
	}

	later := createdAt.Add(48 * time.Hour)
	if err := os.Chtimes(savedPath, later, later); err != nil {
		t.Fatalf("change file times: %v", err)
	}

	listed, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("List() len=%d, want 1", len(listed))
	}
	if !listed[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt=%s, want %s", listed[0].CreatedAt.Format(time.RFC3339Nano), createdAt.Format(time.RFC3339Nano))
	}
}
