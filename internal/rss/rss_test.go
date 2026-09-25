package rss

import (
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
)

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
	// 大小写不敏感
	if !matchRule(rule, "my movie 4K hdr") {
		t.Error("keyword should be case-insensitive")
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
	// matchRule 里 regexp.Compile 失败会直接 return false —— 不会 panic
	if matchRule(rule, "anything") {
		t.Error("invalid regex should result in no match")
	}
}

func TestMatchRule_ExcludeInvalidRegexIsIgnored(t *testing.T) {
	// exclude 的正则编译失败时，应该把 exclude 视为空（不排除任何东西）
	rule := models.RSSRule{Mode: "regex", Include: "movie", Exclude: "["}
	if !matchRule(rule, "movie stuff here") {
		t.Error("invalid exclude regex should not block, only fail include matters")
	}
}
