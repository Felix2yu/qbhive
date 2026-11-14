package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
)

func loadArchive(t *testing.T, json string) models.ArchiveConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(json), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{path: path}
	if err := m.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return m.Get().Archive
}

// 老 config.json 没有 archive 块：两个安全闸门必须补成开启，守卫必须停在 off。
// DryRun 落成 false 会让升级后的第一轮扫描就开始真删任务。
func TestArchiveDefaults_OldConfigGatesOn(t *testing.T) {
	a := loadArchive(t, `{"server":{"listen":":8088"}}`)
	if a.DryRun == nil || !*a.DryRun {
		t.Errorf("缺失的 dryRun 应补成 true，got %v", a.DryRun)
	}
	if a.ExemptLostFiles == nil || !*a.ExemptLostFiles {
		t.Errorf("缺失的 exemptLostFiles 应补成 true，got %v", a.ExemptLostFiles)
	}
	if a.Guard.Mode != models.GuardModeOff {
		t.Errorf("守卫缺省必须是 off，got %q", a.Guard.Mode)
	}
	if a.Interval != 60 || a.BatchSize != 100 || a.QuietMinutes != 5 {
		t.Errorf("数值缺省错误: %+v", a)
	}
	if a.Guard.Interval != 5 || a.Guard.MaxPerRound != 20 || a.Guard.NotifyCooldownMinutes != 10 {
		t.Errorf("守卫数值缺省错误: %+v", a.Guard)
	}
}

func TestArchiveDefaults_ExplicitFalsePreserved(t *testing.T) {
	a := loadArchive(t, `{"archive":{"dryRun":false,"exemptLostFiles":false}}`)
	if a.DryRun == nil || *a.DryRun {
		t.Error("显式 dryRun=false 不该被默认值覆盖")
	}
	if a.ExemptLostFiles == nil || *a.ExemptLostFiles {
		t.Error("显式 exemptLostFiles=false 不该被默认值覆盖")
	}
}

// 配置里打错一个字不该变成「开始删文件」
func TestArchiveDefaults_UnknownGuardModeFallsToOff(t *testing.T) {
	for _, mode := range []string{"", "Off", "notify-files", "delete", "remove-files ", "yes"} {
		a := loadArchive(t, `{"archive":{"guard":{"mode":"`+mode+`"}}}`)
		if a.Guard.Mode != models.GuardModeOff {
			t.Errorf("mode=%q 应降为 off，got %q", mode, a.Guard.Mode)
		}
	}
}

func TestArchiveDefaults_ValidGuardModesPreserved(t *testing.T) {
	for _, mode := range []string{
		models.GuardModeOff, models.GuardModeNotify,
		models.GuardModeRemove, models.GuardModeRemoveFiles,
	} {
		a := loadArchive(t, `{"archive":{"guard":{"mode":"`+mode+`"}}}`)
		if a.Guard.Mode != mode {
			t.Errorf("合法档位 %q 被改成了 %q", mode, a.Guard.Mode)
		}
	}
}

// 做种门槛刻意不补默认值：三个判据都是「<=0 表示不启用」。
// 老配置全为 0 时健康完成任务一条都不退，只有失去实体的那批会退——
// 这是升级后最安全的方向。若在这里补上 1.0/24，等于替用户决定了做种门槛。
func TestArchiveDefaults_SeedingGatesStayDisabled(t *testing.T) {
	a := loadArchive(t, `{"server":{"listen":":8088"}}`)
	if a.MinRatio != 0 || a.MinHours != 0 || a.MinUploadedMB != 0 {
		t.Errorf("老配置的做种门槛应保持 0（不启用），got %+v", a)
	}
}

func TestArchiveDefaults_NewInstallHasStarterGates(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "config.json"))
	a := m.Get().Archive
	if a.MinRatio != 1.0 || a.MinHours != 24 {
		t.Errorf("新装应带起步门槛，got ratio=%v hours=%v", a.MinRatio, a.MinHours)
	}
	if a.Enabled {
		t.Error("新装不该默认启用退休")
	}
	if a.Guard.Mode != models.GuardModeOff {
		t.Errorf("新装守卫应为 off，got %q", a.Guard.Mode)
	}
}

func TestArchiveDefaults_NonPositiveNumbersNormalized(t *testing.T) {
	a := loadArchive(t, `{"archive":{"interval":0,"batchSize":-5,"quietMinutes":0,
		"guard":{"interval":0,"maxPerRound":-1,"notifyCooldownMinutes":0}}}`)
	if a.Interval != 60 || a.BatchSize != 100 || a.QuietMinutes != 5 {
		t.Errorf("非正数应补缺省: %+v", a)
	}
	if a.Guard.Interval != 5 || a.Guard.MaxPerRound != 20 || a.Guard.NotifyCooldownMinutes != 10 {
		t.Errorf("守卫非正数应补缺省: %+v", a.Guard)
	}
}
