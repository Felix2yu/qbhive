package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
)

func TestManager_New_CreatesDefaultWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	m := New(path)
	// New 内部应 Load 失败后写入 defaultConfig
	if m.Get().Qbittorrent.URL != defaultConfig.Qbittorrent.URL {
		t.Errorf("expected default URL %q, got %q", defaultConfig.Qbittorrent.URL, m.Get().Qbittorrent.URL)
	}
	// 文件应已落盘
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file should exist: %v", err)
	}
}

func TestManager_Load_EmptyFileReturnsDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	// 空文件不是"不存在"——Load 能 ReadFile 但 Unmarshal 空 slice 会成功
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(path)
	cfg := m.Get()
	// 空对象 → 零值，不是 defaultConfig（New 里只在 Load 失败时才 defaultConfig）
	if cfg.Qbittorrent.URL != "" {
		t.Errorf("expected empty URL from {} load, got %q", cfg.Qbittorrent.URL)
	}
}

func TestManager_Load_CorruptJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{ not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{path: path}
	err := m.Load()
	if err == nil {
		t.Fatal("expected Load to fail on corrupt JSON")
	}
}

func TestManager_SaveAndLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	m1 := New(path)
	want := models.AppConfig{
		Qbittorrent: models.QBConfig{URL: "http://qb.example.com:8080", Username: "u", Password: "p"},
	}
	m1.Set(want)
	if err := m1.Save(); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// 新 Manager 从同路径 Load
	m2 := &Manager{path: path}
	if err := m2.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	got := m2.Get()
	if got.Qbittorrent.URL != want.Qbittorrent.URL {
		t.Errorf("URL mismatch: got %q want %q", got.Qbittorrent.URL, want.Qbittorrent.URL)
	}
	if got.Qbittorrent.Username != want.Qbittorrent.Username {
		t.Errorf("Username mismatch: got %q want %q", got.Qbittorrent.Username, want.Qbittorrent.Username)
	}
}

func TestManager_Save_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c", "config.json")

	m := &Manager{path: path}
	m.Set(models.AppConfig{Server: models.ServerConfig{Listen: ":9999"}})
	if err := m.Save(); err != nil {
		t.Fatalf("Save should auto-create parent dirs: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config file should exist at deep path: %v", err)
	}
}
