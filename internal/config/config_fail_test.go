package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
)

// TestManager_Load_ZeroLengthFileUsesDefault 覆盖 Load 里 len(data)==0 分支：
// 文件存在但内容为空时，直接采用 defaultConfig 且不报错。
func TestManager_Load_ZeroLengthFileUsesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{path: path}
	if err := m.Load(); err != nil {
		t.Fatalf("空文件 Load 不应报错: %v", err)
	}
	if got := m.Get().Server.Listen; got != defaultConfig.Server.Listen {
		t.Errorf("空文件应回落到默认配置: got listen=%q want %q", got, defaultConfig.Server.Listen)
	}
	if got := m.Get().Qbittorrent.URL; got != defaultConfig.Qbittorrent.URL {
		t.Errorf("空文件应回落到默认配置: got qb.url=%q want %q", got, defaultConfig.Qbittorrent.URL)
	}
}

// TestManager_Save_MkdirAllFails 覆盖 Save 里 MkdirAll 失败分支：
// 父路径中间段是一个普通文件，MkdirAll 必然报 ENOTDIR。
func TestManager_Save_MkdirAllFails(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("i am a file"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &Manager{path: filepath.Join(blocker, "sub", "config.json")}
	m.Set(models.AppConfig{Server: models.ServerConfig{Listen: ":9999"}})
	if err := m.Save(); err == nil {
		t.Fatal("父路径被文件占位时 Save 应返回 MkdirAll 错误")
	}
}

// TestManager_Save_TmpPathIsDir 覆盖 Save 里 WriteFile(tmp) 失败分支：
// 让 <path>.tmp 是一个目录，WriteFile 必然报 EISDIR（不受 root/只读目录影响，跨平台稳定）。
func TestManager_Save_TmpPathIsDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}

	m := &Manager{path: path}
	m.Set(models.AppConfig{Server: models.ServerConfig{Listen: ":9999"}})
	if err := m.Save(); err == nil {
		t.Fatal(".tmp 为目录时 Save 应返回 WriteFile 错误")
	}
	// 正式配置文件不应被创建
	if _, err := os.Stat(path); err == nil {
		t.Error("Save 失败时不应产出正式配置文件")
	}
}

// TestManager_GetSet_RoundTrip 覆盖 Get/Set 的读写路径（带锁的基本读写）。
func TestManager_GetSet_RoundTrip(t *testing.T) {
	m := &Manager{path: filepath.Join(t.TempDir(), "config.json")}
	m.Set(models.AppConfig{
		Qbittorrent: models.QBConfig{URL: "http://127.0.0.1:8080", APIKey: "k"},
		Limiter:     models.LimiterConfig{Enabled: true, Interval: 3},
	})
	got := m.Get()
	if got.Qbittorrent.APIKey != "k" || !got.Limiter.Enabled || got.Limiter.Interval != 3 {
		t.Errorf("Get/Set 往返不一致: %+v", got)
	}
}
