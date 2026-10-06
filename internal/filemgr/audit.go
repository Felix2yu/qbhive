package filemgr

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Felix2yu/qbhive/internal/logger"
)

// ---- 重命名审计日志（append-only JSONL + 状态修正行）----
//
// 文件 data/filemgr_audit.jsonl：每次重命名提交追加一行 committed 记录；
// 下轮确认落地后追加同 ID 的 confirmed 修正行；回退后追加 rolledback 修正行。
// 加载时按顺序解析，同 ID 后出现的行覆盖先前的状态。
// 原始行数超过压缩阈值时整体重写为解析后的最新状态（原子写）。

const auditFile = "filemgr_audit.jsonl"
const auditMaxEntries = 2000       // 内存与文件中保留的最大条目数
const auditCompactThreshold = 4000 // 原始行数超过此值触发压缩重写

// 审计状态
const (
	AuditCommitted  = "committed"  // 已提交 qB renameFile，待确认落地
	AuditConfirmed  = "confirmed"  // 已确认落地
	AuditFailed     = "failed"     // 提交失败或连续多轮未确认后放弃
	AuditRolledback = "rolledback" // 已回退到旧名
	AuditPruned     = "pruned"     // 后处理按大小二选一删除了文件（不可回退）
)

// auditViaPrune 审计记录 via 字段取值：后处理按大小清理（区别于正则 / AI 改名）
const auditViaPrune = "prune"

type auditEntry struct {
	ID      string `json:"id"`
	Ts      int64  `json:"ts"`
	Hash    string `json:"hash"`
	Torrent string `json:"torrent"`
	Old     string `json:"old"`
	New     string `json:"new"`
	Via     string `json:"via"` // regex / ai
	Status  string `json:"status"`
}

type auditLog struct {
	mu       sync.Mutex
	path     string
	entries  []auditEntry // 按追加顺序（旧→新），状态已按修正行解析
	rawLines int
}

// defaultAuditPath 推导审计文件路径（与 scheduler 的 finished.json 同目录规则）
func defaultAuditPath() string {
	if cfg := os.Getenv("QBHIVE_CONFIG"); cfg != "" {
		return filepath.Join(filepath.Dir(cfg), auditFile)
	}
	return filepath.Join("data", auditFile)
}

func newAuditLog(path string) *auditLog {
	a := &auditLog{path: path}
	a.load()
	return a
}

func (a *auditLog) load() {
	f, err := os.Open(a.path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn.Printf("审计日志读取失败：%v", err)
		}
		return
	}
	defer f.Close()
	index := make(map[string]int) // ID → entries 下标
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		a.rawLines++
		if line == "" {
			continue
		}
		var e auditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			logger.Warn.Printf("审计日志存在无法解析的行已跳过：%v", err)
			continue
		}
		if idx, ok := index[e.ID]; ok {
			a.entries[idx].Status = e.Status // 修正行：同 ID 后者覆盖状态
			continue
		}
		index[e.ID] = len(a.entries)
		a.entries = append(a.entries, e)
	}
	// 只保留尾部最近条目
	if len(a.entries) > auditMaxEntries {
		a.entries = append([]auditEntry(nil), a.entries[len(a.entries)-auditMaxEntries:]...)
	}
	// 原始行膨胀则压缩重写
	if a.rawLines > auditCompactThreshold {
		a.rewrite()
	}
	logger.Info.Printf("审计日志已加载 %d 条记录（原始 %d 行）", len(a.entries), a.rawLines)
}

// rewrite 将内存中的解析结果整体重写为紧凑文件（原子写）
func (a *auditLog) rewrite() {
	dir := filepath.Dir(a.path)
	_ = os.MkdirAll(dir, 0o755)
	tmp := a.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Warn.Printf("审计日志压缩失败：%v", err)
		return
	}
	w := bufio.NewWriter(f)
	for _, e := range a.entries {
		line, _ := json.Marshal(e)
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		f.Close()
		logger.Warn.Printf("审计日志压缩失败：%v", err)
		return
	}
	f.Close()
	if err := os.Rename(tmp, a.path); err != nil {
		logger.Warn.Printf("审计日志压缩替换失败：%v", err)
		return
	}
	a.rawLines = len(a.entries)
}

// Append 追加一条记录（自动补 ID / 时间戳），返回带 ID 的记录以便调用方关联
func (a *auditLog) Append(e auditEntry) auditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e.ID == "" {
		e.ID = fmt.Sprintf("%d-%s", time.Now().UnixMilli(), randomHex(4))
	}
	if e.Ts == 0 {
		e.Ts = time.Now().Unix()
	}
	line, err := json.Marshal(e)
	if err == nil {
		dir := filepath.Dir(a.path)
		_ = os.MkdirAll(dir, 0o755)
		f, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(line)
			f.Write([]byte{'\n'})
			f.Close()
			a.rawLines++
		} else {
			logger.Warn.Printf("审计日志写入失败：%v", err)
		}
	} else {
		logger.Warn.Printf("审计日志序列化失败：%v", err)
	}
	a.entries = append(a.entries, e)
	if len(a.entries) > auditMaxEntries {
		a.entries = a.entries[len(a.entries)-auditMaxEntries:]
	}
	if a.rawLines > auditCompactThreshold {
		a.rewrite()
	}
	return e
}

// UpdateStatus 追加同 ID 的状态修正行（confirmed / rolledback / failed）
func (a *auditLog) UpdateStatus(id, status string) bool {
	a.mu.Lock()
	idx := -1
	for i := range a.entries {
		if a.entries[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		a.mu.Unlock()
		return false
	}
	a.entries[idx].Status = status
	e := a.entries[idx]
	a.mu.Unlock()
	a.Append(e) // 追加修正行落盘
	return true
}

// Get 按 ID 查找
func (a *auditLog) Get(id string) (auditEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.entries) - 1; i >= 0; i-- {
		if a.entries[i].ID == id {
			return a.entries[i], true
		}
	}
	return auditEntry{}, false
}

// List 返回倒序（最新在前）分页结果与总数
func (a *auditLog) List(offset, limit int) ([]auditEntry, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	total := len(a.entries)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 50
	}
	// entries 是旧→新；倒序取。
	// 用 make 初始化空切片：无记录时也序列化为 [] 而不是 null，避免前端 null.length 报错
	out := make([]auditEntry, 0, limit)
	for i := total - 1 - offset; i >= 0 && len(out) < limit; i-- {
		out = append(out, a.entries[i])
	}
	return out, total
}
