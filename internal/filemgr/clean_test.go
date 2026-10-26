package filemgr

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
)

func TestCleanBase(t *testing.T) {
	custom := compileCleanRules([]string{`^【测试】\s*`})
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"前缀 www 分隔符", "www.example.com Movie 2023", "Movie 2023"},
		{"前缀 https", "https://down.site.tv Movie", "Movie"},
		{"前缀括号方括号", "[example.com] Movie", "Movie"},
		{"前缀括号全角", "【www.btclub.net】电影名", "电影名"},
		{"前缀括号圆括号", "(www.site.org) Movie", "Movie"},
		{"后缀 @", "Movie@site.com", "Movie"},
		{"后缀全角＠", "Movie＠site.com", "Movie"},
		{"后缀 括号", "Movie [www.example.com]", "Movie"},
		{"后缀 全角括号", "Movie【example.cc】", "Movie"},
		{"叠加多域名", "[a.com] [b.com] Movie", "Movie"},
		{"场景名不误删 S01E01", "S01E01", "S01E01"},
		{"版本号不误删", "v1.2.3", "v1.2.3"},
		{"人名风格不误删", "Mr.Robot", "Mr.Robot"},
		{"双扩展名基础名不误删", "Movie.zh", "Movie.zh"},
		{"场景标记不误删", "Movie.2023.GER", "Movie.2023.GER"},
		{"年份语言标记不误删", "Movie.1999.JP", "Movie.1999.JP"},
		{"技术标记不误删", "Movie.2023.1080p.BluRay.x264", "Movie.2023.1080p.BluRay.x264"},
		{"纯域名保留原名", "www.example.com", "www.example.com"},
		{"自定义正则生效", "【测试】Movie", "Movie"},
		{"普通名不动", "My Great Movie", "My Great Movie"},
		{"独立token域名", "www.a.com.b.com Movie", "Movie"},
		// 域名 + @ 尾巴（下载站水印常见形态，如 98T.la / 489155.com）
		{"后缀 域名带双@", "剧名.第01集.www.98T.la@@", "剧名.第01集"},
		{"后缀 域名带单@", "剧名 第01集 www.98T.la@", "剧名 第01集"},
		{"后缀 纯域名带@", "某视频 489155.com@", "某视频"},
		{"中置 域名带@ 保留后文", "某视频 489155.com@ 1080p", "某视频 1080p"},
		{"前置 域名带双@", "www.98T.la@@ 剧名.S01E01", "剧名.S01E01"},
		{"全角@ 尾巴", "某视频 www.98T.la＠＠", "某视频"},
		{"括号包裹 域名带@", "[www.98T.la@@] 剧名", "剧名"},
		{"叠加 域名带@", "剧名 www.a.com@ www.98T.la@@", "剧名"},
		{"纯水印名回退原名", "www.98T.la@@", "www.98T.la@@"},
		{"年份@不误删", "Movie.2023@1080p", "Movie.2023@1080p"},
		{"语言标记@不误删", "Movie.zh@1080p", "Movie.zh@1080p"},
		{"连字符域名主体不误删", "my-site.top", "my-site.top"},
		// 域名紧贴中文/全角标点：左边界此前只认 ASCII 分隔符，"最新位址：489155.com"
		// 这类广告名整条不命中（全角冒号既不在类里、也不是 RE2 的 \s）
		{"全角冒号前缀域名", "最 新 位 址 獲 取：489155.com 收藏不迷路", "最 新 位 址 獲 取：收藏不迷路"},
		{"半角冒号前缀域名", "最新位址:489155.com 收藏不迷路", "最新位址:收藏不迷路"},
		{"域名紧贴中文左右", "最新地址489155.com收藏不迷路", "最新地址收藏不迷路"},
		{"域名紧贴中文带@尾巴", "最新位址：489155.com@收藏", "最新位址：收藏"},
		{"域名在名尾紧贴全角句号", "剧名第01集489155.com。", "剧名第01集。"},
		{"中文右侧边界不被吃掉", "剧名 www.98T.la最新域名发布", "剧名最新域名发布"},
		// 边界放开后仍须靠 strict TLD 白名单 + looksLikeDomain 挡住场景命名
		{"中文紧邻非白名单TLD不误删", "我们的故事.Me", "我们的故事.Me"},
		{"中文紧邻语言标记不误删", "剧集.ZH", "剧集.ZH"},
		{"场景链右侧中文不误删", "Movie.x264中文", "Movie.x264中文"},
		{"中文后的英文组标域名删掉", "某番剧Fansub.cc 第01集", "某番剧第01集"},
		{"中文点分段后接白名单TLD不误删", "行尸走肉.S01E01.top集", "行尸走肉.S01E01.top集"},
		// emoji / 颜文字清理
		{"emoji 后缀", "Movie 🔥🔥", "Movie"},
		{"emoji 前后夹", "🔥Movie🔥HD", "MovieHD"},
		{"emoji 中置合并空格", "Movie 🔥 Sub", "Movie Sub"},
		{"星号符号清理", "Movie ★★★", "Movie"},
		{"商标符号清理", "Movie™ ©2023", "Movie 2023"},
		{"中文夹emoji", "电影🔥名", "电影名"},
		{"颜文字掷桌", "Movie (╯°□°）╯︵ ┻━┻", "Movie"},
		{"颜文字圆脸", "(◕‿◕) Movie", "Movie"},
		{"括号年份保留", "Movie (2023)", "Movie (2023)"},
		{"括号字母保留", "Movie (Extended)", "Movie (Extended)"},
		{"全角括号数字保留", "【4K】Movie ★", "【4K】Movie"},
		{"假名不误删", "Movie ツ", "Movie ツ"},
		{"韩文括号保留", "Movie (한국어)", "Movie (한국어)"},
		{"纯emoji保留原名", "🔥🔥", "🔥🔥"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanBase(c.in, custom); got != c.want {
				t.Errorf("cleanBase(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSanitizeCleaned(t *testing.T) {
	cases := []struct {
		name string
		orig string
		in   string
		want string
	}{
		{"空结果回退原名", "Movie", "", "Movie"},
		{"非法字符替换", "Movie", `a/b:c*d`, "a_b_c_d"},
		{"控制字符丢弃", "Movie", "a\x01b", "ab"},
		{"与原名仅大小写不同回退", "Movie", "movie", "Movie"},
		{"超长回退原名", "Movie", string(make([]byte, 250)), "Movie"},
		{"首尾空白与点清理", "Movie", "  Movie. ", "Movie"},
		{"正常结果保留", "Movie (2023)", "Movie (2023) clean", "Movie (2023) clean"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeCleaned(c.orig, c.in); got != c.want {
				t.Errorf("sanitizeCleaned(%q, %q) = %q, want %q", c.orig, c.in, got, c.want)
			}
		})
	}
}

func TestLooksLikeDomain(t *testing.T) {
	yes := []string{"example.com", "www.example.com", "sub.site.tv", "my-site.top"}
	no := []string{"S01E01", "v1.2.3", "Movie.zh", "abc", "Movie.2023", "site.123", "1080p.BluRay"}
	for _, d := range yes {
		if !looksLikeDomain(d) {
			t.Errorf("looksLikeDomain(%q) = false, want true", d)
		}
	}
	for _, d := range no {
		if looksLikeDomain(d) {
			t.Errorf("looksLikeDomain(%q) = true, want false", d)
		}
	}
}

// touchCleanSource 在 savePath 下创建清洗计划的「源文件」实体：
// buildCleanPlan 有实体存在性护栏（僵尸任务磁盘无文件时跳过），测试须先落盘。
func touchCleanSource(t *testing.T, savePath, rel string) {
	t.Helper()
	p := filepath.Join(savePath, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildCleanPlan(t *testing.T) {
	savePath := t.TempDir()
	files := []models.QBFile{
		{Name: "[site.com] Movie/sub/file one.mkv"},
		{Name: "[site.com] Movie/sub/file two.mkv"},
		{Name: "[site.com] Movie/readme.txt"},
	}
	for _, f := range files {
		touchCleanSource(t, savePath, f.Name)
	}
	plan := buildCleanPlan(files, nil, nil, savePath, "abcdef1234567890")
	// qB renameFile 不认目录段（409），目录改名必须落成「每个文件一条」的全路径移动
	if len(plan) != 3 {
		t.Fatalf("plan length = %d, want 3: %+v", len(plan), plan)
	}
	want := [][2]string{
		{"[site.com] Movie/sub/file one.mkv", "Movie/sub/file one.mkv"},
		{"[site.com] Movie/sub/file two.mkv", "Movie/sub/file two.mkv"},
		{"[site.com] Movie/readme.txt", "Movie/readme.txt"},
	}
	for i, w := range want {
		if plan[i].Old != w[0] || plan[i].New != w[1] {
			t.Errorf("plan[%d] = %v, want %v → %v", i, plan[i], w[0], w[1])
		}
	}
}

// 清洗结果若会把路径逃出保存根（. / .. 段），必须整条跳过
func TestBuildCleanPlanEscapeGuard(t *testing.T) {
	savePath := t.TempDir()
	files := []models.QBFile{{Name: "正常名/../../evil.mkv"}}
	plan := buildCleanPlan(files, nil, nil, savePath, "abcdef1234567890")
	// "../../evil" 里的 ".." 段：cleanBase 不改动 → newFull 含 ".." → 护栏拒绝
	for _, r := range plan {
		if r.New == "../../evil.mkv" {
			t.Errorf("越界目标不应进入计划: %+v", plan)
		}
	}
}

func TestBuildCleanPlanDedupAndConflict(t *testing.T) {
	savePath := t.TempDir()
	files := []models.QBFile{
		{Name: "www.a.com Movie.mkv"},
		{Name: "www.b.com Movie.mkv"}, // 两个文件清洗后同名 → 第二条加 _hash8
	}
	for _, f := range files {
		touchCleanSource(t, savePath, f.Name)
	}
	plan := buildCleanPlan(files, nil, nil, savePath, "abcdef1234567890")
	if len(plan) != 2 {
		t.Fatalf("plan length = %d, want 2: %+v", len(plan), plan)
	}
	if plan[0].New != "Movie.mkv" {
		t.Errorf("plan[0].New = %q, want Movie.mkv", plan[0].New)
	}
	if plan[1].New != "Movie_abcdef12.mkv" {
		t.Errorf("plan[1].New = %q, want Movie_abcdef12.mkv（计划内目标去重兜底）", plan[1].New)
	}
}

func TestBuildCleanPlanDiskConflict(t *testing.T) {
	savePath := t.TempDir()
	// 预先放置同名文件，绝不覆盖 → 改用 _hash8 兜底
	if err := os.WriteFile(filepath.Join(savePath, "Movie.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []models.QBFile{{Name: "www.a.com Movie.mkv"}}
	touchCleanSource(t, savePath, "www.a.com Movie.mkv")
	plan := buildCleanPlan(files, nil, nil, savePath, "abcdef1234567890")
	if len(plan) != 1 {
		t.Fatalf("plan length = %d, want 1: %+v", len(plan), plan)
	}
	if plan[0].New != "Movie_abcdef12.mkv" {
		t.Errorf("plan[0].New = %q, want Movie_abcdef12.mkv（磁盘冲突兜底）", plan[0].New)
	}
}

func TestBuildCleanPlanAISkip(t *testing.T) {
	savePath := t.TempDir()
	files := []models.QBFile{{Name: "www.a.com Movie.mkv"}}
	touchCleanSource(t, savePath, "www.a.com Movie.mkv")
	aiNames := map[string]string{"Movie": "The Movie (Clean)"} // 以 post-regex base 为键
	plan := buildCleanPlan(files, nil, aiNames, savePath, "abcdef1234567890")
	if len(plan) != 1 || plan[0].New != "The Movie (Clean).mkv" || plan[0].Via != "ai" {
		t.Errorf("plan = %+v, want The Movie (Clean).mkv via ai", plan)
	}
}

// 僵尸任务护栏：qB 文件列表里的路径在磁盘上不存在（存储已脱钩）时，
// renameFile 只会改内部元数据、文件永远洗不到，必须整条跳过
func TestBuildCleanPlanSourceMissingSkip(t *testing.T) {
	savePath := t.TempDir()
	files := []models.QBFile{{Name: "www.a.com Movie.mkv"}} // 不落盘
	plan := buildCleanPlan(files, nil, nil, savePath, "abcdef1234567890")
	if len(plan) != 0 {
		t.Fatalf("plan = %+v, want 空（源文件不在磁盘上必须跳过）", plan)
	}
}
