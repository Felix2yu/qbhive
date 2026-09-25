package web

import (
	"strings"
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
)

func TestMaskAppriseURL(t *testing.T) {
	// 真实逻辑：全程脱敏，只要有 scheme 就脱敏 rest；无 scheme 时短的直接 "...********"
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"http://short", "http://...********"},                 // rest=5 ≤6 也脱敏
		{"http://a", "http://...********"},                     // rest=1 同上
		{"http://long-secret-token", "http://lon...********"},  // rest>6 → 保留前3
		{"tgram://bottoken/ChatID", "tgram://bot...********"},  // 协议 + 前3 + ...********
		{"no-scheme-short", "no-...********"},                  // 无 scheme + len>6 → 前3 + ...
		{"abc", "...********"},                                 // 无 scheme + len≤6 → 纯掩码
		{"1234567890abcd", "123...********"},                   // 无 scheme 长于 6
	}
	for _, c := range cases {
		got := maskAppriseURL(c.in)
		if got != c.want {
			t.Errorf("maskAppriseURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// emptyAppCfg 返回一个完全合法、零错误的 AppConfig，用例按需覆写字段
func emptyAppCfg() *models.AppConfig {
	return &models.AppConfig{
		Qbittorrent: models.QBConfig{URL: "http://127.0.0.1:8080"},
		RSS: models.RSSConfig{
			Enabled:  true,
			Interval: 15,
			Feeds: []models.RSSFeed{
				{ID: "f1", Name: "feed", URL: "https://example.com/feed.xml", Enabled: true},
			},
		},
		Limiter: models.LimiterConfig{
			Enabled:  true,
			Interval: 10,
			Rules: []models.LimitRule{
				{ID: "r1", Name: "rule", Enabled: true, Match: ".*\\.mkv$"},
			},
		},
		FileManager: models.FileManagerConfig{Enabled: true, ScanInterval: 60},
		Notifier: models.NotifierConfig{Enabled: true, AppriseURLs: []string{"tgram://abc/123"}},
	}
}

func TestValidateConfig_HappyPath(t *testing.T) {
	if msg := validateConfig(emptyAppCfg()); msg != "" {
		t.Errorf("expected empty message on valid config, got: %s", msg)
	}
}

func TestValidateConfig_QBUrlMustBeHTTP(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.Qbittorrent.URL = "ftp://qb.example.com"
	if msg := validateConfig(cfg); msg == "" {
		t.Fatal("expected error for non-http QB URL")
	} else if !strings.Contains(msg, "http:// 或 https://") {
		t.Errorf("unexpected msg: %s", msg)
	}
}

func TestValidateConfig_QBUrlHTTPSOK(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.Qbittorrent.URL = "https://qb.example.com:8080"
	if msg := validateConfig(cfg); msg != "" {
		t.Errorf("https URL should be valid, got: %s", msg)
	}
}

func TestValidateConfig_EmptyQBUrlAllowed(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.Qbittorrent.URL = ""
	if msg := validateConfig(cfg); msg != "" {
		t.Errorf("empty QB URL should be allowed (unconfigured), got: %s", msg)
	}
}

func TestValidateConfig_RSSIntervalZero(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.RSS.Interval = 0
	if msg := validateConfig(cfg); !strings.Contains(msg, "RSS 刷新间隔必须 ≥ 1") {
		t.Errorf("expected RSS interval error, got: %s", msg)
	}
	// 禁用 RSS 时不校验 interval
	cfg.RSS.Enabled = false
	cfg.RSS.Interval = 0
	if msg := validateConfig(cfg); msg != "" {
		t.Errorf("disabled RSS should skip interval check, got: %s", msg)
	}
}

func TestValidateConfig_LimiterIntervalZero(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.Limiter.Interval = 0
	if msg := validateConfig(cfg); !strings.Contains(msg, "限速器刷新间隔必须 ≥ 1") {
		t.Errorf("expected limiter interval error, got: %s", msg)
	}
}

func TestValidateConfig_FileManagerIntervalZero(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.FileManager.ScanInterval = 0
	if msg := validateConfig(cfg); !strings.Contains(msg, "文件管理扫描间隔必须 ≥ 1") {
		t.Errorf("expected filemanager interval error, got: %s", msg)
	}
}

func TestValidateConfig_AppriseURLMissingScheme(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.Notifier.AppriseURLs = []string{"tgram://ok", "missing-scheme", ""}
	if msg := validateConfig(cfg); !strings.Contains(msg, "缺少协议前缀") {
		t.Errorf("expected apprise scheme error, got: %s", msg)
	}
}

// 覆盖 validateConfig 里 "空串 continue" 分支 —— 必须让空串出现在不会触发 return 的位置之后
func TestValidateConfig_AppriseURLEmptyStringIsSkipped(t *testing.T) {
	cfg := emptyAppCfg()
	// 空串打头（触发 continue），合法 URL 跟尾，整体应通过
	cfg.Notifier.AppriseURLs = []string{"", "tgram://bottoken/ChatID"}
	if msg := validateConfig(cfg); msg != "" {
		t.Errorf("empty apprise URL should be skipped (continue branch), got: %s", msg)
	}
}

func TestValidateConfig_RSSFeedURLNotHTTP(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.RSS.Feeds[0].URL = "ftp://feed.xml"
	if msg := validateConfig(cfg); !strings.Contains(msg, "RSS 订阅源 URL 必须以 http 开头") {
		t.Errorf("expected RSS feed URL error, got: %s", msg)
	}
}

func TestValidateConfig_RuleIncludeInvalidRegex(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.RSS.Feeds[0].Rules = []models.RSSRule{
		{ID: "r1", Name: "bad", Mode: "regex", Include: "["},
	}
	if msg := validateConfig(cfg); !strings.Contains(msg, "include 正则无效") {
		t.Errorf("expected include regex error, got: %s", msg)
	}
}

func TestValidateConfig_RuleExcludeInvalidRegex(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.RSS.Feeds[0].Rules = []models.RSSRule{
		{ID: "r1", Name: "bad", Mode: "regex", Include: "ok", Exclude: "("},
	}
	if msg := validateConfig(cfg); !strings.Contains(msg, "exclude 正则无效") {
		t.Errorf("expected exclude regex error, got: %s", msg)
	}
}

func TestValidateConfig_LimiterRuleInvalidRegex(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.Limiter.Rules = []models.LimitRule{
		{ID: "r1", Name: "bad", Match: "*unclosed"},
	}
	if msg := validateConfig(cfg); !strings.Contains(msg, "match 正则无效") {
		t.Errorf("expected limiter match regex error, got: %s", msg)
	}
}

// keyword 模式下即使 Include 是 "*" 也不当正则编译——应通过
func TestValidateConfig_RSSRuleKeywordModeSkipsRegexCompile(t *testing.T) {
	cfg := emptyAppCfg()
	cfg.RSS.Feeds[0].Rules = []models.RSSRule{
		{ID: "r1", Name: "kw", Mode: "keyword", Include: "[invalid-regex"},
	}
	if msg := validateConfig(cfg); msg != "" {
		t.Errorf("keyword mode should skip regex compile, got: %s", msg)
	}
}
