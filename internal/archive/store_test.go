package archive

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func rec(hash, name string, completedOn int64) Record {
	return Record{
		Hash: hash, Name: name, SavePath: "/data/" + name,
		Size: 1000, AddedOn: completedOn - 100, CompletedOn: completedOn,
		Magnet: "magnet:?xt=urn:btih:" + hash, Reason: ReasonLostFiles,
	}
}

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), storeFile)
	return NewStore(path), path
}

func TestStore_AppendThenReload(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.Append(rec("a", "Alpha", 100)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// 追加即落盘：退休流程「先建档后删任务」依赖这一点，
	// 不能只在内存里可见
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("账本文件应已存在: %v", err)
	}
	if !strings.Contains(string(raw), `"hash":"a"`) {
		t.Fatalf("文件里没有刚追加的记录: %s", raw)
	}

	reloaded := NewStore(path)
	got, ok := reloaded.Get("a")
	if !ok || got.Name != "Alpha" {
		t.Fatalf("重启后应读到记录，got %+v ok=%v", got, ok)
	}
}

func TestStore_CorrectionLineOverrides(t *testing.T) {
	s, path := newTestStore(t)
	if err := s.Append(rec("a", "Alpha", 100)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.MarkRetired("a", 500); err != nil {
		t.Fatalf("MarkRetired: %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("修正行不该新增记录，Len=%d", s.Len())
	}
	got, _ := s.Get("a")
	if got.RetiredAt != 500 {
		t.Errorf("RetiredAt=%d want 500", got.RetiredAt)
	}

	reloaded := NewStore(path)
	got, ok := reloaded.Get("a")
	if !ok || got.RetiredAt != 500 || got.Name != "Alpha" {
		t.Fatalf("重启后修正行应整条覆盖，got %+v", got)
	}
}

// 写失败必须返回错误且内存状态不变：退休流程靠这个返回值决定能不能删任务。
// 若失败还更新内存，就会出现「账本里没有、任务却已被删」的不可逆丢失。
func TestStore_AppendFailureLeavesMemoryUntouched(t *testing.T) {
	dir := t.TempDir()
	// 把「账本路径」指向一个目录：OpenFile 必然失败
	s := NewStore(dir)
	if err := s.Append(rec("a", "Alpha", 100)); err == nil {
		t.Fatal("路径是目录时 Append 应失败")
	}
	if s.Len() != 0 {
		t.Errorf("写失败不该更新内存，Len=%d", s.Len())
	}
	if _, ok := s.Get("a"); ok {
		t.Error("写失败后不该能查到记录")
	}
}

func TestStore_AppendRejectsEmptyHash(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.Append(Record{Name: "no hash"}); err == nil {
		t.Fatal("缺 hash 应拒绝")
	}
	if s.Len() != 0 {
		t.Errorf("Len=%d want 0", s.Len())
	}
}

func TestStore_RemoveIsEscapeHatch(t *testing.T) {
	s, path := newTestStore(t)
	_ = s.Append(rec("a", "Alpha", 100))
	_ = s.Append(rec("b", "Beta", 200))
	if err := s.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok := s.Get("a"); ok {
		t.Error("移除后不该查到")
	}
	if s.Len() != 1 {
		t.Errorf("Len=%d want 1", s.Len())
	}
	// 重写落盘：重启后不能复活
	reloaded := NewStore(path)
	if _, ok := reloaded.Get("a"); ok {
		t.Error("重启后被移除的记录复活了")
	}
	if _, ok := reloaded.Get("b"); !ok {
		t.Error("不该误删其它记录")
	}
	if err := s.Remove("nope"); err == nil {
		t.Error("移除不存在的记录应报错")
	}
}

func TestStore_SearchFilters(t *testing.T) {
	s, _ := newTestStore(t)
	a := rec("a", "Movie.4k.mkv", 100)
	a.Category, a.Tags, a.SavePath = "电影", "转码, 4k", "/data/电影/Movie.4k.mkv"
	b := rec("b", "Show.1080p.mkv", 200)
	b.Category, b.Tags = "剧集", "转码"
	c := rec("c", "Movie.720p.mkv", 300)
	c.Category, c.SavePath = "电影", "/data/电影/Movie.720p.mkv"
	c.RetiredAt = 0
	for _, r := range []Record{a, b, c} {
		if err := s.Append(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	_ = s.MarkRetired("a", 900)
	_ = s.MarkRetired("b", 900)

	cases := []struct {
		name string
		q    Query
		want []string
	}{
		{"关键词匹配名称", Query{Q: "movie"}, []string{"c", "a"}},
		{"关键词大小写不敏感", Query{Q: "MOVIE"}, []string{"c", "a"}},
		{"关键词匹配保存路径", Query{Q: "/data/电影"}, []string{"c", "a"}},
		{"分类精确匹配", Query{Category: "电影"}, []string{"c", "a"}},
		{"分类不匹配子串", Query{Category: "电"}, nil},
		{"标签命中其一", Query{Tag: "4k"}, []string{"a"}},
		{"标签大小写不敏感", Query{Tag: "4K"}, []string{"a"}},
		{"标签不误命中子串", Query{Tag: "4"}, nil},
		{"完成时间下界", Query{From: 200}, []string{"c", "b"}},
		{"完成时间上界", Query{To: 200}, []string{"b", "a"}},
		{"只看候选", Query{Pending: true}, []string{"c"}},
		{"组合条件", Query{Category: "电影", Pending: true}, []string{"c"}},
		{"无命中", Query{Q: "不存在"}, nil},
	}
	for _, c := range cases {
		got, total := s.Search(c.q)
		var hashes []string
		for _, r := range got {
			hashes = append(hashes, r.Hash)
		}
		if strings.Join(hashes, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v want %v", c.name, hashes, c.want)
		}
		if total != len(c.want) {
			t.Errorf("%s: total=%d want %d", c.name, total, len(c.want))
		}
	}
}

func TestStore_SearchSortAndPagination(t *testing.T) {
	s, _ := newTestStore(t)
	for i, name := range []string{"a", "b", "c", "d"} {
		_ = s.Append(rec(name, name, int64(100+i)))
	}
	got, total := s.Search(Query{Sort: "completedOn", Order: "asc", Offset: 1, Limit: 2})
	if total != 4 {
		t.Fatalf("total=%d want 4", total)
	}
	if len(got) != 2 || got[0].Hash != "b" || got[1].Hash != "c" {
		t.Fatalf("升序分页错误: %+v", got)
	}
	got, _ = s.Search(Query{Sort: "completedOn", Offset: 0, Limit: 2})
	if len(got) != 2 || got[0].Hash != "d" || got[1].Hash != "c" {
		t.Fatalf("默认降序错误: %+v", got)
	}
	got, _ = s.Search(Query{Sort: "name", Order: "asc"})
	if len(got) != 4 || got[0].Hash != "a" || got[3].Hash != "d" {
		t.Fatalf("按名称排序错误: %+v", got)
	}
	// 越界偏移返回空而不是 panic
	got, total = s.Search(Query{Offset: 99})
	if len(got) != 0 || total != 4 {
		t.Fatalf("越界偏移: len=%d total=%d", len(got), total)
	}
}

// 排序值相同时必须靠 hash 兜底：map 遍历顺序随机，
// 没有稳定末级键，翻页时同值记录会来回跳。
func TestStore_SearchStableTiebreak(t *testing.T) {
	s, _ := newTestStore(t)
	for _, h := range []string{"e", "a", "d", "b", "c"} {
		_ = s.Append(rec(h, h, 100)) // 完成时间全相同
	}
	var first []string
	for i := 0; i < 5; i++ {
		got, _ := s.Search(Query{Limit: 100})
		var hs []string
		for _, r := range got {
			hs = append(hs, r.Hash)
		}
		if i == 0 {
			first = hs
			continue
		}
		if strings.Join(hs, ",") != strings.Join(first, ",") {
			t.Fatalf("同排序值的顺序不稳定：%v vs %v", hs, first)
		}
	}
	if strings.Join(first, ",") != "a,b,c,d,e" {
		t.Errorf("同值应按 hash 升序兜底，got %v", first)
	}
}

// 无结果时必须是 [] 而不是 null，否则前端 null.length 报错
func TestStore_SearchEmptyIsNotNull(t *testing.T) {
	s, _ := newTestStore(t)
	got, total := s.Search(Query{})
	if got == nil {
		t.Fatal("空结果应返回非 nil 切片")
	}
	if total != 0 {
		t.Errorf("total=%d want 0", total)
	}
	line, _ := json.Marshal(got)
	if string(line) != "[]" {
		t.Errorf("序列化应为 []，got %s", line)
	}
}

func TestStore_SearchLimitCapped(t *testing.T) {
	s, _ := newTestStore(t)
	_ = s.Append(rec("a", "a", 100))
	got, _ := s.Search(Query{Limit: 100000})
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	// 上限本身生效：造 maxLimit+1 条，请求超大 limit 也只给 maxLimit 条
	for i := 0; i < maxLimit+1; i++ {
		_ = s.Append(rec("bulk-"+strconv.Itoa(i), "n", int64(i)))
	}
	got, total := s.Search(Query{Limit: 100000})
	if len(got) != maxLimit {
		t.Errorf("单页应被截到 %d，got %d", maxLimit, len(got))
	}
	if total != maxLimit+2 {
		t.Errorf("total 应是命中总数 %d，got %d", maxLimit+2, total)
	}
}

func TestStore_CompactRewrite(t *testing.T) {
	s, path := newTestStore(t)
	// 同一 hash 反复修正，堆出远超记录数的原始行
	for i := 0; i < 30; i++ {
		_ = s.Append(rec("a", "Alpha", int64(100+i)))
	}
	before := countLines(t, path)
	if before != 30 {
		t.Fatalf("压缩前应有 30 行，got %d", before)
	}
	s.rawLines = storeCompactThreshold + 1
	if err := s.Append(rec("a", "Alpha", 999)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	after := countLines(t, path)
	if after != 1 {
		t.Errorf("超阈值应压缩为 1 行，got %d", after)
	}
	reloaded := NewStore(path)
	got, ok := reloaded.Get("a")
	if !ok || got.CompletedOn != 999 {
		t.Fatalf("压缩后应保留最新状态，got %+v", got)
	}
}

func TestStore_SkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, storeFile)
	content := `{"hash":"a","name":"Alpha","completedOn":100}
这不是 JSON
{"hash":"","name":"无 hash"}

{"hash":"b","name":"Beta","completedOn":200}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入: %v", err)
	}
	s := NewStore(path)
	if s.Len() != 2 {
		t.Fatalf("应只加载 2 条合法记录，Len=%d", s.Len())
	}
	if _, ok := s.Get("a"); !ok {
		t.Error("坏行不该影响好行")
	}
}

func TestStore_MissingFileIsEmpty(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "不存在.jsonl"))
	if s.Len() != 0 {
		t.Errorf("Len=%d want 0", s.Len())
	}
	got, total := s.Search(Query{})
	if len(got) != 0 || total != 0 || got == nil {
		t.Errorf("空账本检索: %+v total=%d", got, total)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", path, err)
	}
	return strings.Count(string(raw), "\n")
}
