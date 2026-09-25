package rss

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
)

// setupEngine 在临时目录创建 RSS Engine，QBHIVE_CONFIG 指向 config.json
func setupEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)
	return New(nil, nil)
}

func TestDefaultStatePath_UsesEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "sub", "config.json")
	t.Setenv("QBHIVE_CONFIG", cfgPath)
	got := defaultStatePath()
	want := filepath.Join(dir, "sub", stateFileName)
	if got != want {
		t.Errorf("rss defaultStatePath with env = %q, want %q", got, want)
	}
}

func TestDefaultStatePath_FallbackDataDir(t *testing.T) {
	t.Setenv("QBHIVE_CONFIG", "")
	got := defaultStatePath()
	if got != filepath.Join("data", stateFileName) {
		t.Errorf("rss defaultStatePath fallback = %q", got)
	}
}

func TestSaveAndLoadSeen_RoundTrip(t *testing.T) {
	e := setupEngine(t)
	e.stateFile = filepath.Join(t.TempDir(), "rss_seen.json")
	e.seen["feed1"] = map[string]bool{"g1": true, "g2": true}
	e.seen["feed2"] = map[string]bool{"g3": true}
	e.saveSeen()

	// 新 Engine 从同文件加载
	e2 := setupEngine(t)
	e2.stateFile = e.stateFile
	e2.seen = make(map[string]map[string]bool)
	e2.loadSeen()

	if _, ok := e2.seen["feed1"]; !ok {
		t.Fatalf("feed1 missing after loadSeen, got %v", e2.seen)
	}
	if !e2.seen["feed1"]["g1"] || !e2.seen["feed1"]["g2"] {
		t.Errorf("feed1 entries wrong: %v", e2.seen["feed1"])
	}
	if !e2.seen["feed2"]["g3"] {
		t.Errorf("feed2 entries wrong: %v", e2.seen["feed2"])
	}
}

func TestLoadSeen_MissingFile(t *testing.T) {
	e := setupEngine(t)
	e.stateFile = filepath.Join(t.TempDir(), "nope.json")
	e.seen = make(map[string]map[string]bool)
	e.loadSeen() // 不 panic
}

func TestLoadSeen_CorruptJSON(t *testing.T) {
	e := setupEngine(t)
	tmp := t.TempDir()
	e.stateFile = filepath.Join(tmp, "bad.json")
	_ = os.WriteFile(e.stateFile, []byte("{ not json"), 0o644)
	e.seen = make(map[string]map[string]bool)
	e.loadSeen() // 不 panic
	if len(e.seen) != 0 {
		t.Errorf("seen should stay empty on corrupt JSON, got %v", e.seen)
	}
}

func TestSaveSeen_CreatesParentDir(t *testing.T) {
	e := setupEngine(t)
	tmp := t.TempDir()
	e.stateFile = filepath.Join(tmp, "a", "b", "rss_seen.json")
	e.seen["x"] = map[string]bool{"y": true}
	e.saveSeen()
	if _, err := os.Stat(e.stateFile); err != nil {
		t.Fatalf("saveSeen should auto-create dirs: %v", err)
	}
}

func TestIsFreshFeed(t *testing.T) {
	e := setupEngine(t)
	if !e.isFreshFeed("never") {
		t.Error("never-seen feed should be fresh")
	}
	e.seen["f"] = map[string]bool{}
	if !e.isFreshFeed("f") {
		t.Error("empty seen map should be fresh")
	}
	e.seen["f"]["g"] = true
	if e.isFreshFeed("f") {
		t.Error("feed with entries should NOT be fresh")
	}
}

// ============== matchRule (保留原有测试 + 新边界) ==============

func TestMatchRule_EmptyRule(t *testing.T) {
	rule := models.RSSRule{Mode: "keyword", Include: "", Exclude: ""}
	if matchRule(rule, "anything") {
		t.Error("empty include + empty exclude should never match")
	}
}

func TestMatchRule_KeywordInclude(t *testing.T) {
	rule := models.RSSRule{Mode: "keyword", Include: "4k", Exclude: ""}
	if !matchRule(rule, "My.Movie.4K.HDR.2160p.mkv") {
		t.Error("should match case-insensitive include")
	}
	if matchRule(rule, "My.Movie.1080p.mkv") {
		t.Error("should not match missing keyword")
	}
}

func TestMatchRule_KeywordExclude(t *testing.T) {
	rule := models.RSSRule{Mode: "keyword", Include: "", Exclude: "4k"}
	if !matchRule(rule, "My.Movie.1080p.mkv") {
		t.Error("empty include should pass through")
	}
	if matchRule(rule, "My.Movie.4K.HDR.mkv") {
		t.Error("exclude should block matches")
	}
}

func TestMatchRule_KeywordIncludeAndExclude(t *testing.T) {
	rule := models.RSSRule{Mode: "keyword", Include: "batman", Exclude: "extras"}
	if !matchRule(rule, "Batman (2022).Remux.mkv") {
		t.Error("include batman, exclude extras -> should match")
	}
	if matchRule(rule, "Batman.Extras.Disc1.mkv") {
		t.Error("exclude extras should block")
	}
}

func TestMatchRule_RegexInclude(t *testing.T) {
	rule := models.RSSRule{Mode: "regex", Include: `\b\d{4}p\b`, Exclude: ""}
	if !matchRule(rule, "Movie.2160p.mkv") {
		t.Error("regex include should match 2160p")
	}
	if matchRule(rule, "Movie.BDRip.mkv") {
		t.Error("regex include should not match BDRip")
	}
}

func TestMatchRule_RegexExclude(t *testing.T) {
	rule := models.RSSRule{Mode: "regex", Include: "", Exclude: `(?i:remux|internal)`}
	if matchRule(rule, "Movie.2160p.REMUX.mkv") {
		t.Error("regex exclude should block REMUX")
	}
	if !matchRule(rule, "Movie.2160p.WEB-DL.mkv") {
		t.Error("regex exclude should let WEB-DL pass")
	}
}

func TestMatchRule_RegexInvalidCompileReturnsFalse(t *testing.T) {
	rule := models.RSSRule{Mode: "regex", Include: "["}
	if matchRule(rule, "anything") {
		t.Error("invalid regex should result in no match")
	}
}

func TestMatchRule_ExcludeInvalidRegexIsIgnored(t *testing.T) {
	rule := models.RSSRule{Mode: "regex", Include: "movie", Exclude: "["}
	if !matchRule(rule, "movie stuff here") {
		t.Error("invalid exclude regex should not block, only include matters")
	}
}
