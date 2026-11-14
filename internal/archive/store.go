// Package archive 任务退休账本。
//
// 退休＝把完成并收敛的任务从 qB 移除（磁盘文件保留），元数据写进账本仍可检索。
// 账本同时是重复下载守卫的判据来源：qB 里出现的 infohash 若已在账本里且
// 添加时刻晚于退休时刻，就是「退休后又重下」。
//
// 存储沿用 filemgr 审计日志的 append-only JSONL + 同键修正行 + 原子重写，
// 但有两处刻意不同：不设条目上限（历史库就是几千上万条），且追加必须 Sync——
// 「先建档、后删任务」的顺序依赖这条记录真的落盘。
package archive

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Felix2yu/qbhive/internal/logger"
)

const storeFile = "archive.jsonl"

// storeCompactThreshold 原始行数超过此值触发压缩重写。
// 每条退休记录会写两行（建档 + 填 RetiredAt 的修正行），阈值要留出这个倍数。
const storeCompactThreshold = 20000

// 退休原因
const (
	ReasonLostFiles = "lost-files" // 原片已被转码清理，任务只剩空壳
	ReasonRatio     = "ratio"      // 分享比率达标
	ReasonHours     = "hours"      // 做种时长达标
	ReasonUploaded  = "uploaded"   // 上传量达标
	ReasonManual    = "manual"     // 用户在历史页手动退休
)

// Record 一条归档记录。只存元数据与磁链，不存 .torrent 二进制、不解析转码产物路径。
type Record struct {
	Hash        string  `json:"hash"`
	Name        string  `json:"name"`
	Category    string  `json:"category,omitempty"`
	Tags        string  `json:"tags,omitempty"`
	SavePath    string  `json:"savePath"`
	ContentPath string  `json:"contentPath,omitempty"` // 原路径：原片可能已被清理，不保证仍存在
	Tracker     string  `json:"tracker,omitempty"`
	Magnet      string  `json:"magnet"`
	Size        int64   `json:"size"`
	Uploaded    int64   `json:"uploaded"`
	AddedOn     int64   `json:"addedOn"`
	CompletedOn int64   `json:"completedOn"`
	Ratio       float64 `json:"ratio"`
	// RetiredAt 为 0 表示已建档但任务还没从 qB 移除（dryRun 候选）；
	// 大于 0 表示已退休，守卫据此判定重复下载
	RetiredAt int64  `json:"retiredAt,omitempty"`
	Reason    string `json:"reason"`
}

// Query 检索条件。零值表示不过滤；分页与服务端排序都在这里表达，
// 几千条记录不整表下发给前端。
type Query struct {
	Q        string // 大小写不敏感子串，匹配 Name 与 SavePath
	Category string // 精确匹配
	Tag      string // 命中 Tags 里的任一标签
	From     int64  // CompletedOn >= From，0 表示不限
	To       int64  // CompletedOn <= To，0 表示不限
	Pending  bool   // 只看已建档未退休的候选
	Sort     string // name / size / addedOn / completedOn / retiredAt，默认 completedOn
	Order    string // asc / desc，默认 desc
	Offset   int
	Limit    int
}

// maxLimit 单页上限：挡住「一次把整本账本拉走」的请求
const maxLimit = 1000

type Store struct {
	mu       sync.Mutex
	path     string
	records  map[string]Record
	rawLines int
}

// DefaultPath 账本文件路径，与 config.json 同目录（随容器卷持久化）
func DefaultPath() string {
	if cfg := os.Getenv("QBHIVE_CONFIG"); cfg != "" {
		return filepath.Join(filepath.Dir(cfg), storeFile)
	}
	return filepath.Join("data", storeFile)
}

func NewStore(path string) *Store {
	s := &Store{path: path, records: make(map[string]Record)}
	s.load()
	return s
}

func (s *Store) load() {
	f, err := os.Open(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn.Printf("归档账本读取失败：%v", err)
		}
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		s.rawLines++
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			logger.Warn.Printf("归档账本存在无法解析的行已跳过：%v", err)
			continue
		}
		if r.Hash == "" {
			continue
		}
		s.records[r.Hash] = r // 修正行：同 hash 后者整条覆盖
	}
	if err := scanner.Err(); err != nil {
		logger.Warn.Printf("归档账本扫描中断：%v", err)
	}
	if s.rawLines > storeCompactThreshold {
		s.rewrite()
	}
	logger.Info.Printf("归档账本已加载 %d 条记录（原始 %d 行）", len(s.records), s.rawLines)
}

// rewrite 把内存里的最新状态整体重写为紧凑文件（原子写）。
// 调用方必须已持锁。
func (s *Store) rewrite() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, r := range s.records {
		line, err := json.Marshal(r)
		if err != nil {
			continue
		}
		_, _ = w.Write(line)
		_ = w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.rawLines = len(s.records)
	return nil
}

// Append 追加一条记录并落盘。
//
// 返回错误时内存状态不变：退休流程靠这个返回值决定「能不能删任务」，
// 写失败却更新了内存，就会出现账本里没有、任务却已被删的不可逆丢失。
func (s *Store) Append(r Record) error {
	if r.Hash == "" {
		return fmt.Errorf("归档记录缺少 hash")
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	// 必须 Sync：退休是「先建档后删任务」，只写进页缓存就删任务，
	// 掉电后账本里没有这条记录，任务却已经没了
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	s.records[r.Hash] = r
	s.rawLines++
	if s.rawLines > storeCompactThreshold {
		if err := s.rewrite(); err != nil {
			logger.Warn.Printf("归档账本压缩失败：%v", err)
		}
	}
	return nil
}

// MarkRetired 追加同 hash 的修正行，填上退休时刻
func (s *Store) MarkRetired(hash string, ts int64) error {
	s.mu.Lock()
	r, ok := s.records[hash]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("归档记录不存在：%s", hash)
	}
	r.RetiredAt = ts
	return s.Append(r)
}

// Remove 从账本彻底移除一条记录（原子重写整个文件）。
// 这是「我确实想重下这个磁链」的逃生口：记录没了，守卫就不再拦。
// 先重写成功再改内存，避免重写失败后内存与文件不一致。
func (s *Store) Remove(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[hash]; !ok {
		return fmt.Errorf("归档记录不存在：%s", hash)
	}
	delete(s.records, hash)
	if err := s.rewrite(); err != nil {
		// 重写失败：把内存恢复回去，下次重启仍能从文件读到这条
		s.load()
		return err
	}
	return nil
}

func (s *Store) Get(hash string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[hash]
	return r, ok
}

func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// Search 服务端过滤 + 排序 + 分页，返回命中总数（不是本页条数）
func (s *Store) Search(q Query) ([]Record, int) {
	s.mu.Lock()
	all := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		all = append(all, r)
	}
	s.mu.Unlock()

	needle := strings.ToLower(strings.TrimSpace(q.Q))
	category := strings.TrimSpace(q.Category)
	tag := strings.ToLower(strings.TrimSpace(q.Tag))
	filtered := make([]Record, 0, len(all))
	for _, r := range all {
		if needle != "" &&
			!strings.Contains(strings.ToLower(r.Name), needle) &&
			!strings.Contains(strings.ToLower(r.SavePath), needle) {
			continue
		}
		if category != "" && r.Category != category {
			continue
		}
		if tag != "" && !hasTag(r.Tags, tag) {
			continue
		}
		if q.From > 0 && r.CompletedOn < q.From {
			continue
		}
		if q.To > 0 && r.CompletedOn > q.To {
			continue
		}
		if q.Pending && r.RetiredAt != 0 {
			continue
		}
		filtered = append(filtered, r)
	}

	sortRecords(filtered, q.Sort, strings.EqualFold(q.Order, "asc"))

	total := len(filtered)
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	// 用 make 初始化空切片：无结果时也序列化为 [] 而不是 null
	out := make([]Record, 0, limit)
	for i := offset; i < total && len(out) < limit; i++ {
		out = append(out, filtered[i])
	}
	return out, total
}

// hasTag 判断逗号分隔的标签串里是否含某个标签（大小写不敏感）
func hasTag(tags, want string) bool {
	for _, t := range strings.Split(tags, ",") {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// sortRecords 排序并以 hash 兜底比较：map 遍历顺序随机，
// 没有稳定的末级键，排序值相同的记录会在翻页时来回跳。
func sortRecords(list []Record, field string, asc bool) {
	var less func(a, b Record) bool
	switch field {
	case "name":
		less = func(a, b Record) bool { return strings.ToLower(a.Name) < strings.ToLower(b.Name) }
	case "size":
		less = func(a, b Record) bool { return a.Size < b.Size }
	case "addedOn":
		less = func(a, b Record) bool { return a.AddedOn < b.AddedOn }
	case "retiredAt":
		less = func(a, b Record) bool { return a.RetiredAt < b.RetiredAt }
	default:
		less = func(a, b Record) bool { return a.CompletedOn < b.CompletedOn }
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if less(a, b) != less(b, a) {
			if asc {
				return less(a, b)
			}
			return less(b, a)
		}
		return a.Hash < b.Hash
	})
}
