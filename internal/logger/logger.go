// Package logger 提供全局分级日志：四把 logger 照旧写 stdout/stderr，
// 同时把每一行抄进内存环形缓冲，供应用内的「日志」页读取。
package logger

import (
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RingSize 是环形缓冲保留的日志条数，超出后覆盖最旧的一条。
const RingSize = 2000

// BootID 标识当前进程。重启后 Seq 从头计数，光看 Seq 分不出「新日志」还是
// 「上一代的旧日志」，前端凭 BootID 变化整份重拉。
var BootID = strconv.FormatInt(time.Now().UnixNano(), 36)

// Entry 是一条已归档的日志。Seq 全局单调递增，前端靠它做增量拉取；
// TS 是 Unix 秒，与重命名审计日志同一口径，行序仍以 Seq 为准。
type Entry struct {
	Seq   uint64 `json:"seq"`
	TS    int64  `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

type ring struct {
	mu   sync.Mutex
	buf  []Entry
	head int
	seq  uint64
}

var global = &ring{buf: make([]Entry, 0, RingSize)}

func (r *ring) add(level, msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	e := Entry{Seq: r.seq, TS: time.Now().Unix(), Level: level, Msg: msg}
	if len(r.buf) < cap(r.buf) {
		r.buf = append(r.buf, e)
		return
	}
	r.buf[r.head] = e
	r.head = (r.head + 1) % cap(r.buf)
}

// Query 取 Seq 大于 since 的日志，按写入顺序（旧→新）返回，最多 limit 条；
// 命中超出 limit 时保留最近的。cursor 是本次返回的最后一条 Seq（无命中则原样
// 返回 since），前端下一轮以它为 since —— 既不会重复也不会漏。
// 级别 / 关键字筛选交给调用方，缓冲只有一份。
func Query(since uint64, limit int) (entries []Entry, cursor uint64) {
	if limit <= 0 || limit > RingSize {
		limit = RingSize
	}
	global.mu.Lock()
	defer global.mu.Unlock()
	entries = make([]Entry, 0, limit)
	n := len(global.buf)
	// head 指向最旧一条；未满时 head 恒为 0，顺序读一圈即是写入顺序
	for i := 0; i < n; i++ {
		e := global.buf[(global.head+i)%n]
		if e.Seq > since {
			entries = append(entries, e)
		}
	}
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	cursor = since
	if len(entries) > 0 {
		cursor = entries[len(entries)-1].Seq
	}
	return entries, cursor
}

// capture 是挂在 log.Logger 输出端的旁路写手。log 包把行拼成
// "[INFO]  2026/01/02 15:04:05 正文"（LstdFlags：级别前缀在前，时间头固定 20 字符），
// 这里两级都剥掉只留正文 —— 时间已经单独记在 Entry.TS，stdout 的格式一个字符都不动。
type capture struct {
	level string
	head  string
}

const stampLen = len("2006/01/02 15:04:05 ")

func hasStampHeader(s string) bool {
	return len(s) >= stampLen && s[4] == '/' && s[7] == '/' && s[10] == ' ' && s[13] == ':' && s[16] == ':'
}

func (c capture) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\r\n")
	if i := strings.Index(msg, c.head); i >= 0 {
		msg = msg[i+len(c.head):]
	}
	if hasStampHeader(msg) {
		msg = msg[stampLen:]
	}
	global.add(c.level, msg)
	return len(p), nil
}

func newLogger(level, prefix string, out io.Writer) *log.Logger {
	return log.New(io.MultiWriter(out, capture{level: level, head: prefix}), prefix, log.LstdFlags)
}

var (
	Info  = newLogger("info", "[INFO]  ", os.Stdout)
	Warn  = newLogger("warn", "[WARN]  ", os.Stdout)
	Error = newLogger("error", "[ERROR] ", os.Stderr)
	Debug = newLogger("debug", "[DEBUG] ", os.Stdout)
)
