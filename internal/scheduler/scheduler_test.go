package scheduler

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
)

func TestIsDoneState(t *testing.T) {
	done := []string{"pausedUP", "stalledUP", "uploading", "checkingUP", "queuedUP"}
	for _, s := range done {
		if !isDoneState(s) {
			t.Errorf("expected %q to be done", s)
		}
	}
	notDone := []string{"downloading", "stalledDL", "metaDL", "checkingDL", "queuedDL", "forcedDL", "forcedUP", "missingFiles", "error", ""}
	for _, s := range notDone {
		if isDoneState(s) {
			t.Errorf("expected %q NOT to be done", s)
		}
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1024 * 1024, "1.00 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
		{1024 * 1024 * 1024 * 1024, "1.00 TB"},
	}
	for _, c := range cases {
		got := humanSize(c.in)
		if got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// setupScheduler 在临时目录里创建一个 Scheduler，QBHIVE_CONFIG 指向 config.json
func setupScheduler(t *testing.T) *Scheduler {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)
	return New(nil, nil, nil, nil, nil, nil)
}

func TestDefaultStatePath_UsesEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "sub", "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)
	got := defaultStatePath()
	want := filepath.Join(dir, "sub", finishedFile)
	if got != want {
		t.Errorf("defaultStatePath with env = %q, want %q", got, want)
	}
}

func TestDefaultStatePath_FallbackDataDir(t *testing.T) {
	t.Setenv("QBHIVE_CONFIG", "")
	got := defaultStatePath()
	if got != filepath.Join("data", finishedFile) {
		t.Errorf("defaultStatePath fallback = %q, want data/%s", got, finishedFile)
	}
}

func TestSaveAndLoadFinished_RoundTrip(t *testing.T) {
	s := setupScheduler(t)

	// 先存几个 hash
	s.finished["aaa"] = true
	s.finished["bbb"] = true
	s.saveFinished()
	if _, err := os.Stat(s.stateFile); err != nil {
		t.Fatalf("state file should exist after saveFinished: %v", err)
	}

	// 新 Scheduler 应从同路径加载回来
	s2 := setupScheduler(t)
	// 用 os.Rename 让 s2 拿到同文件（setupScheduler 每次用不同 TempDir）
	// 简单点：直接构造 s2 的 stateFile 指向 s 的
	s2.stateFile = s.stateFile
	s2.finished = make(map[string]bool)
	s2.loadFinished()

	if !s2.finished["aaa"] || !s2.finished["bbb"] {
		t.Errorf("loadFinished missing hashes, got %v", s2.finished)
	}
}

func TestLoadFinished_MissingFileIsNoOp(t *testing.T) {
	s := setupScheduler(t)
	// 默认 stateFile 不存在，loadFinished 应该什么都不做不 panic
	s.stateFile = filepath.Join(t.TempDir(), "does-not-exist.json")
	s.finished = make(map[string]bool)
	s.loadFinished() // 不 panic 就是通过
}

func TestLoadFinished_CorruptJSONIsNoOp(t *testing.T) {
	s := setupScheduler(t)
	tmp := t.TempDir()
	s.stateFile = filepath.Join(tmp, "bad.json")
	_ = os.WriteFile(s.stateFile, []byte("{ not json"), 0o644)
	s.finished = make(map[string]bool)
	s.loadFinished() // 不 panic，finished 保持空
	if len(s.finished) != 0 {
		t.Errorf("finished should stay empty on corrupt JSON, got %v", s.finished)
	}
}

func TestSaveFinished_CreatesParentDir(t *testing.T) {
	s := setupScheduler(t)
	tmp := t.TempDir()
	s.stateFile = filepath.Join(tmp, "a", "b", "c", "finished.json")
	s.finished["x"] = true
	s.saveFinished()
	if _, err := os.Stat(s.stateFile); err != nil {
		t.Fatalf("saveFinished should auto-create dirs: %v", err)
	}
}

func TestBuildCompletedBody_IncludesAllFields(t *testing.T) {
	body := buildCompletedBody(models.QBTorrent{
		Name: "My.Movie.4K.mkv", Size: 1024 * 1024 * 1024,
		Category: "Movies", Tags: "4k,hdr",
		Hash: "abc123", SavePath: "/downloads/",
		AddedOn: 1700000000, CompletedOn: 1700086400,
	})
	for _, want := range []string{"📦", "My.Movie.4K.mkv", "Movies", "abc123", "Movies", "1.00 GB"} {
		if !contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	// 零值时间应渲染为 "-"
	zeroBody := buildCompletedBody(models.QBTorrent{Name: "x", Size: 0})
	if !contains(zeroBody, "-") {
		t.Errorf("zero time should render '-': %s", zeroBody)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && indexOf(s, sub) >= 0 }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
