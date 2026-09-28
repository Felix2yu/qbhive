package filemgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/models"
)

func TestValidateBatchTemplate(t *testing.T) {
	ok := []string{"{index}_{orig}", "{date} {type} {index}", "{title} - {orig}.{index}", "{orig}"}
	for _, tpl := range ok {
		if err := validateBatchTemplate(tpl); err != nil {
			t.Errorf("validateBatchTemplate(%q) = %v, want nil", tpl, err)
		}
	}
	bad := []struct{ tpl, want string }{
		{"", "不能为空"},
		{"{name}_{index}", "未知变量 {name}"},
		{"固定名", "须包含"},
		{"{date} {type}", "须包含"},
	}
	for _, c := range bad {
		err := validateBatchTemplate(c.tpl)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("validateBatchTemplate(%q) = %v, want 含 %q", c.tpl, err, c.want)
		}
	}
	if err := validateBatchTemplate(strings.Repeat("a", 250) + "_{index}"); err == nil {
		t.Error("超长模板未报错")
	}
}

func TestExpandBatchTemplate(t *testing.T) {
	got := expandBatchTemplate("{date}_{type}_{index}_{orig}_{title}",
		"Old Movie", "Some.Torrent", "2026-09-28", "video", 3)
	want := "2026-09-28_video_3_Old Movie_Some.Torrent"
	if got != want {
		t.Errorf("expand = %q, want %q", got, want)
	}
}

func TestBatchTypeOf(t *testing.T) {
	cases := map[string]string{
		"mkv": "video", "MKV": "video", ".mp3": "audio", "jpg": "image",
		"zip": "archive", "pdf": "doc", "xyz": "other", "": "other",
	}
	for ext, want := range cases {
		if got := batchTypeOf(ext); got != want {
			t.Errorf("batchTypeOf(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestBuildBatchPlan(t *testing.T) {
	savePath := t.TempDir()
	tor := models.QBTorrent{
		Hash:        "abcdef1234567890",
		Name:        "Some.Torrent",
		SavePath:    savePath,
		CompletedOn: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC).Unix(),
	}
	files := []models.QBFile{
		{Name: "sub dir/www.a.com Movie.mkv"},
		{Name: "sub dir/www.b.com Movie.mkv"}, // 同目录，模板无变化变量区分 → 第二条 _hash8
	}
	plan := buildBatchPlan(files, tor, "{orig} (2026)")
	// 两个文件 orig 均为 "www.a.com Movie"/"www.b.com Movie"，不会撞名
	if len(plan) != 2 {
		t.Fatalf("plan length = %d, want 2: %+v", len(plan), plan)
	}
	if plan[0].Old != "sub dir/www.a.com Movie.mkv" || plan[0].New != "sub dir/www.a.com Movie (2026).mkv" {
		t.Errorf("plan[0] = %+v", plan[0])
	}
	if plan[0].Via != "manual" {
		t.Errorf("via = %q, want manual", plan[0].Via)
	}
	for _, p := range plan {
		if strings.Contains(p.New, "already") {
			t.Errorf("模板产出与原名相同的文件不应出现在计划中: %+v", p)
		}
	}
}

func TestBuildBatchPlanNoChangeSkip(t *testing.T) {
	savePath := t.TempDir()
	tor := models.QBTorrent{Hash: "abcdef1234567890", Name: "T", SavePath: savePath, CompletedOn: 1700000000}
	files := []models.QBFile{
		{Name: "already fine.mkv"},
		{Name: "sub/another one.txt"},
	}
	// {orig} 展开后与原名一致 → 整个计划为空
	if plan := buildBatchPlan(files, tor, "{orig}"); len(plan) != 0 {
		t.Errorf("plan = %+v, want 空（模板产出与原名相同全部跳过）", plan)
	}
}

func TestBuildBatchPlanIndexAndType(t *testing.T) {
	savePath := t.TempDir()
	tor := models.QBTorrent{
		Hash:        "abcdef1234567890",
		Name:        "Some.Torrent",
		SavePath:    savePath,
		CompletedOn: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC).Unix(),
	}
	files := []models.QBFile{
		{Name: "www.a.com Movie.mkv"},
		{Name: "www.b.com Song.flac"},
		{Name: "photo.png"},
	}
	plan := buildBatchPlan(files, tor, "{date}_{type}_{index}")
	if len(plan) != 3 {
		t.Fatalf("plan length = %d, want 3: %+v", len(plan), plan)
	}
	wantNews := map[string]string{
		"www.a.com Movie.mkv": "2026-09-28_video_1.mkv",
		"www.b.com Song.flac": "2026-09-28_audio_2.flac",
		"photo.png":           "2026-09-28_image_3.png",
	}
	for _, p := range plan {
		if wantNews[p.Old] != p.New {
			t.Errorf("plan entry %q → %q, want %q", p.Old, p.New, wantNews[p.Old])
		}
	}
}

func TestBuildBatchPlanDirSegmentUntouched(t *testing.T) {
	savePath := t.TempDir()
	tor := models.QBTorrent{Hash: "abcdef1234567890", Name: "T", SavePath: savePath, CompletedOn: 1700000000}
	files := []models.QBFile{{Name: "Season 1/www.a.com S01E01.mkv"}}
	plan := buildBatchPlan(files, tor, "E{index}")
	if len(plan) != 1 {
		t.Fatalf("plan length = %d, want 1: %+v", len(plan), plan)
	}
	if plan[0].New != "Season 1/E1.mkv" {
		t.Errorf("plan[0].New = %q, want 'Season 1/E1.mkv'（目录段不动）", plan[0].New)
	}
}

func TestBuildBatchPlanConflictFallback(t *testing.T) {
	savePath := t.TempDir()
	if err := os.WriteFile(filepath.Join(savePath, "E1.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tor := models.QBTorrent{Hash: "abcdef1234567890", Name: "T", SavePath: savePath, CompletedOn: 1700000000}
	files := []models.QBFile{{Name: "www.a.com Movie.mkv"}}
	plan := buildBatchPlan(files, tor, "E{index}")
	if len(plan) != 1 || plan[0].New != "E1_abcdef12.mkv" {
		t.Errorf("plan = %+v, want 磁盘冲突兜底 E1_abcdef12.mkv", plan)
	}
}

func TestBuildBatchPlanDateFallback(t *testing.T) {
	savePath := t.TempDir()
	tor := models.QBTorrent{
		Hash:     "abcdef1234567890",
		Name:     "T",
		SavePath: savePath,
		AddedOn:  time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC).Unix(),
	}
	files := []models.QBFile{{Name: "a.mkv"}}
	plan := buildBatchPlan(files, tor, "{date}_{index}")
	if len(plan) != 1 || plan[0].New != "2026-01-02_1.mkv" {
		t.Errorf("plan = %+v, want CompletedOn 缺失时回退 AddedOn", plan)
	}
}
