package filemgr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Felix2yu/qbhive/internal/logger"
	"github.com/Felix2yu/qbhive/internal/models"
)

// ---- 下载完成后的外部命令后处理 ----
//
// 典型用法是调用 macOS 快捷指令转码（Permute）：
//
//	/usr/bin/shortcuts run "Permute HEVC 50% 缩放" -i "$FILE"
//
// 触发点固定为「单文件自动归档落地之后」——归档走 qB renameFile 时磁盘移动是
// 异步的，只有当 finishFlatten 确认原目录已清空、或确认本就是单文件种子时，
// 目标文件的最终路径才确定，此时才允许执行外部命令。

const (
	// postMaxAttempts 单次启动内，一条命令失败后的最大尝试次数（退避重试，重启不重来）
	postMaxAttempts = 3
	// postLogTailLines 命令输出写入日志的最大行数（避免转码工具刷屏）
	postLogTailLines = 15
	// postTestTimeout 设置页「测试运行」的兜底超时（配置里留 0 或超长时用它收紧，
	// 避免测试请求一直挂着）。正常自动执行不受此限制。
	postTestTimeout = 120 * time.Second
	// postMinOutputRatio 产物判定下限：产物小于源文件该比例时视为可疑半成品
	// （转码中途失败留下的小文件），此时不删源文件，避免误删完好的原片。
	postMinOutputRatio = 0.05
)

// postProbeTimeout ffprobe 校验的超时（秒）。只解析文件头部元数据，正常是亚秒级，
// 给足 10s 足够；超时即判为校验失败（视为产物可疑），安全侧不动任何文件。
const postProbeTimeout = 10

// postProbePaths ffprobe 的回退探测路径：Homebrew / Intel Mac / 系统包管理器常见落点
var postProbePaths = []string{"/opt/homebrew/bin/ffprobe", "/usr/local/bin/ffprobe", "/usr/bin/ffprobe"}

// postVideoExts 产物发现时只认这些扩展名，避免把字幕、封面图等无关文件算进来
var postVideoExts = map[string]bool{
	"mp4": true, "mov": true, "m4v": true, "mkv": true, "avi": true,
	"webm": true, "mpg": true, "mpeg": true, "ts": true, "wmv": true, "flv": true,
}

// postEnvNames 语句渲染出的环境变量名 → 占位符映射（值原样交给 shell，由使用者加引号）
var postEnvNames = []string{"FILE", "DIR", "NAME", "CATEGORY", "SAVEPATH"}

// postRetryDelay 每次失败后的重试退避基数（第 n 次失败后等 n×该值），乘上后按
// 剩余超时裁短。用 var 而非 const：单测要把它调小才能秒级跑完重试用例。
var postRetryDelay = 20 * time.Second

// postEnabled 后处理是否已开启
func (m *Manager) postEnabled() bool {
	return m.cfg.Get().FileManager.PostProcess.Enabled
}

// handlePostProcess 在归档落地之后触发一次后处理。
//
// 返回 (done, retry)：
//
//	done  — 本任务已无待办（未开启、已执行过、无匹配目标、命令正常启动），
//	        调用方据此把任务标 done，之后不再进入处理
//	retry — 暂缓（典型场景：归档还在等 qB 异步移动落地），调用方先不标 done 以便下轮再试。
//	        命令本身失败时的重试在后台 goroutine 内退避完成，不走这条路径
func (m *Manager) handlePostProcess(t models.QBTorrent) (done bool, retry bool) {
	pc := m.cfg.Get().FileManager.PostProcess
	if !pc.Enabled {
		return true, false
	}
	// 归档还在等待 qB 异步移动落地（原目录未清空）时必须让路：
	// 此时文件还没到最终落点，任何转码都是作用在旧路径上。
	if dir := m.getFlattenDir(t.Hash); dir != "" {
		logger.Debug.Printf("后处理 [%s] 归档待落地（%s），让路等下轮", t.Name, dir)
		return false, true
	}

	m.mu.Lock()
	rec := m.cleanState[t.Hash]
	if rec == nil {
		rec = &cleanRecord{}
		m.cleanState[t.Hash] = rec
	}
	if rec.PostDone {
		m.mu.Unlock()
		return true, false
	}
	m.mu.Unlock()

	// 目标文件：归档已完成，content_path 此时要么是文件本身（单文件种子），
	// 要么是只含唯一文件的目录（qB 异步移动落地后的落点）
	file, ok := m.resolveTarget(t)
	if !ok {
		// 没有唯一目标文件（多文件种子、目录里还有别的东西）：不重试，记日志
		logger.Debug.Printf("后处理 [%s] 未解析到唯一目标文件（content=%s），跳过", t.Name, t.ContentPath)
		m.markPostDone(t.Hash, "")
		return true, false
	}
	if !matchExt(file, pc.Extensions) {
		logger.Debug.Printf("后处理 [%s] %s 的扩展名不在白名单（%q），跳过", t.Name, file, pc.Extensions)
		m.markPostDone(t.Hash, file)
		return true, false
	}
	if strings.TrimSpace(pc.Command) == "" {
		logger.Warn.Printf("后处理 [%s] 已启用但未配置命令，跳过 %s", t.Name, file)
		m.markPostDone(t.Hash, file)
		return true, false
	}

	// 命令可能跑很久（转码动辄几十分钟），后台执行，绝不阻塞扫描 ticker。
	// 启动即标记 postDone：重启不重复执行，失败由命令内部的退避重试兜底。
	m.markPostDone(t.Hash, file)
	go m.runPostProcess(t, file, pc)
	return true, false
}

// markPostDone 记录某任务的后处理已派发并落盘（file 仅用于日志与排查）
func (m *Manager) markPostDone(hash, file string) {
	m.mu.Lock()
	if rec := m.cleanState[hash]; rec != nil {
		rec.PostDone = true
		rec.PostAttempts = 0
		rec.PostFile = file
	}
	m.mu.Unlock()
	m.saveCleanState()
}

// resolveTarget 解析本次后处理的目标文件绝对路径。
// 优先用 5.x 的 content_path：单文件种子它是文件本身，多文件种子是内容目录；
// 目录内必须恰好一个可见普通文件才认（归档落地后的落点就是这种情况），
// 否则不做任何猜测——宁可不处理，也不能拿错路径去转码。
func (m *Manager) resolveTarget(t models.QBTorrent) (string, bool) {
	dir := ""
	if t.ContentPath != "" {
		if fi, err := os.Stat(t.ContentPath); err == nil {
			if fi.Mode().IsRegular() {
				return t.ContentPath, true
			}
			if fi.IsDir() {
				dir = t.ContentPath
			}
		}
	}
	// content_path 取不到时的回退：保存根（单文件种子的 save_path 即文件所在目录）
	if dir == "" {
		dir = t.SavePath
	}
	if dir == "" {
		return "", false
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	var vis []os.DirEntry
	for _, e := range entries {
		if e.Name()[0] == '.' || !e.Type().IsRegular() {
			continue
		}
		vis = append(vis, e)
	}
	if len(vis) != 1 {
		return "", false
	}
	return filepath.Join(dir, vis[0].Name()), true
}

// runPostProcess 后台执行后处理命令（成功后的清理在命令内部调用）
func (m *Manager) runPostProcess(t models.QBTorrent, file string, pc models.PostProcessConfig) {
	runPostCommandWithRetry(t, file, pc, func(tt models.QBTorrent, src string, started time.Time) {
		m.postPrune(tt, src, started, pc)
	})
}

// postPruneFunc 命令成功后的清理回调，签名与 Manager.postPrune 一致，
// 抽成类型是为了让重试循环不依赖 Manager（单测可直接替换成计数器）。
type postPruneFunc func(models.QBTorrent, string, time.Time)

// runPostCommandWithRetry 反复尝试执行 pc.Command，返回是否至少成功一次。
//
// 重试是「失败后退避再跑一次」的循环：pc.Timeout 是**总预算**（从第一次尝试起算），
// 每次尝试与重试等待都会被它裁短，超时即放弃，避免一条卡死的命令永远转下去。
// prune 只在成功那一次被调用（成功后的大小二选一清理）。
func runPostCommandWithRetry(t models.QBTorrent, file string, pc models.PostProcessConfig, prune postPruneFunc) bool {
	// 总预算截止时间；零值（未配超时）表示不限
	var deadline time.Time
	if pc.Timeout > 0 {
		deadline = time.Now().Add(time.Duration(pc.Timeout) * time.Second)
	}
	// 返回 (剩余预算, 传给 execPostCommand 的秒数)。left<0 表示「不限时」，
	// 与「已超时应直接放弃」的 left==0 严格区分——早先两者混成 0，导致
	// 没配超时的任务第一次尝试被当成超时跳过。
	budget := func() (time.Duration, int) {
		if deadline.IsZero() {
			return -1, 0 // 不限时
		}
		left := time.Until(deadline)
		if left <= 0 {
			return 0, 0
		}
		sec := int(left.Seconds())
		if sec < 1 {
			sec = 1
		}
		return left, sec
	}

	var lastErr error
	for attempt := 1; attempt <= postMaxAttempts; attempt++ {
		left, sec := budget()
		if left == 0 {
			logger.Warn.Printf("后处理 [%s] 超过总超时 %ds，第 %d 次尝试跳过", t.Name, pc.Timeout, attempt)
			return false
		}
		cmdStr, err := renderPostCommand(pc.Command, t, file)
		if err != nil {
			logger.Warn.Printf("后处理 [%s] 命令模板渲染失败，跳过：%v", t.Name, err)
			return false
		}
		started := time.Now()
		code, output, err := execPostCommand(cmdStr, t, file, sec)
		logPostOutput(t.Name, output)
		if err != nil {
			lastErr = err
			logger.Warn.Printf("后处理 [%s] 第 %d/%d 次执行失败（退出码 %d）：%v", t.Name, attempt, postMaxAttempts, code, err)
		} else if code != 0 {
			lastErr = fmt.Errorf("退出码 %d", code)
			logger.Warn.Printf("后处理 [%s] 第 %d/%d 次执行返回非 0（退出码 %d）", t.Name, attempt, postMaxAttempts, code)
		} else {
			logger.Info.Printf("后处理 [%s] 命令执行完成，用时 %s", t.Name, time.Since(started).Round(time.Second))
			if pc.SizePrune && prune != nil {
				prune(t, file, started)
			}
			return true
		}
		// 退避：第 n 次失败后等 n×base，且不超过剩余预算（不限时则按完整等待）
		if attempt < postMaxAttempts {
			wait := postRetryDelay * time.Duration(attempt)
			if left > 0 && wait > left {
				wait = left
			}
			if wait > 0 {
				time.Sleep(wait)
			}
		}
	}
	logger.Warn.Printf("后处理 [%s] %s 连续 %d 次失败，放弃（修改命令后可在设置页用「测试运行」重跑）：%v",
		t.Name, file, postMaxAttempts, lastErr)
	return false
}

// renderPostCommand 把单行命令模板渲染成可交给 /bin/sh -c 的字符串。
// {} 占位符替换成**单引号包裹的字面量**（路径含空格、单引号、中文都安全），
// 用户也可以直接用 $FILE 等环境变量自己控制引号。
func renderPostCommand(tpl string, t models.QBTorrent, file string) (string, error) {
	s := strings.TrimSpace(tpl)
	if s == "" {
		return "", fmt.Errorf("命令为空")
	}
	dir := filepath.Dir(file)
	repl := []struct{ k, v string }{
		{"{file}", file},
		{"{dir}", dir},
		{"{name}", t.Name},
		{"{category}", t.Category},
		{"{savepath}", t.SavePath},
	}
	for _, r := range repl {
		s = strings.ReplaceAll(s, r.k, shellSingleQuote(r.v))
	}
	return s, nil
}

// shellSingleQuote 生成 POSIX 单引号字面量：单引号本身写成 '\”，其余原样。
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// execPostCommand 执行 shell 命令，返回退出码、合并输出与错误。
// Timeout<=0 表示不限；超时用 context 兜底（超时时退出码记为 -1）。
func execPostCommand(cmdStr string, t models.QBTorrent, file string, timeout int) (int, string, error) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdStr)
	// 环境变量取原始值（未经 shell 转义），使用者在命令里自行加引号即可
	cmd.Env = append(os.Environ(),
		"FILE="+file,
		"DIR="+filepath.Dir(file),
		"NAME="+t.Name,
		"CATEGORY="+t.Category,
		"SAVEPATH="+t.SavePath,
	)
	if t.SavePath != "" {
		cmd.Dir = t.SavePath
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	// 退出码：命令被超时 context 杀掉时 err 为 context 错误，记为 -1 与「非 0」区分开
	code := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		code = -1
	}
	return code, buf.String(), err
}

// logPostOutput 把命令的合并输出按行写日志（只保留尾部若干行）
func logPostOutput(torrent, output string) {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(output) == "" {
		return
	}
	start := 0
	if len(lines) > postLogTailLines {
		start = len(lines) - postLogTailLines
	}
	for _, l := range lines[start:] {
		logger.Info.Printf("后处理 [%s] %s", torrent, strings.TrimRight(l, "\r"))
	}
}

// findProbeBinary 定位 ffprobe：优先用显式配置的 ProbePath（存在即可用，不校验可执行位，
//
//	permission 问题留给执行阶段报错），否则依次查 PATH 和常见安装路径。
//
// 第二个返回值表示「校验器到底可不可用」——不可用时必须退化为「不删任何文件」。
func findProbeBinary(probePath string) (string, bool) {
	if probePath != "" {
		if p, err := filepath.Abs(probePath); err == nil {
			if _, err := os.Stat(p); err == nil {
				return p, true
			}
		}
		return "", false
	}
	if p, err := exec.LookPath("ffprobe"); err == nil && p != "" {
		return p, true
	}
	for _, p := range postProbePaths {
		if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}

// probeVideoFile 用 ffprobe 判断一个文件是否真是「解得出视频流」的视频。
//
// 返回：
//
//	ok     — 校验器跑通且文件里至少有一路视频流（可以放心当转码产物用）
//	detail — 给日志看的身份信息（编码 / 分辨率）或失败原因
//
// 注意退出码不能单独当成功标准：没有视频流的文件 ffprobe 也常常返回 0，
// 所以以能否解析出 codec_type=video 为准。
func probeVideoFile(bin, file string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), postProbeTimeout*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name,codec_type,width,height",
		"-of", "json",
		file,
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	detail := strings.TrimSpace(buf.String())
	if err != nil {
		reason := err.Error()
		if detail != "" {
			reason = detail
		}
		return false, "ffprobe 执行失败：" + strings.Split(reason, "\n")[0]
	}
	type streamInfo struct {
		CodecName string `json:"codec_name"`
		CodecType string `json:"codec_type"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	}
	var out struct {
		Streams []streamInfo `json:"streams"`
	}
	if e := json.Unmarshal(buf.Bytes(), &out); e != nil {
		return false, "ffprobe 输出解析失败：" + e.Error()
	}
	for _, s := range out.Streams {
		if s.CodecType == "video" {
			if s.Width > 0 && s.Height > 0 {
				return true, fmt.Sprintf("%s %dx%d", s.CodecName, s.Width, s.Height)
			}
			return true, s.CodecName
		}
	}
	return false, "ffprobe 未解出视频流"
}

// probeCandidate 一个待评估的产物（大小 + 校验结果）
type probeCandidate struct {
	path    string
	size    int64
	ok      bool   // 通过 ffprobe 校验；未做校验时默认有效（走纯大小判定的兼容模式）
	checked bool   // 是否真的跑过 ffprobe
	detail  string // 编码/分辨率，或失败原因
}

// pickPruneAction 在校验过的产物上决定保留谁、删谁（纯函数，便于单测覆盖）。
//
//	所有产物都无效（校验失败或校验器不可用）→ 一个都不删，只留日志
//	存在有效产物：
//	   最小有效产物 < 源片 → 删源片留产物（仍是半成品比例则两边都不删）
//	   最小有效产物 >= 源片 → 删产物留源片
//
// 无效产物一律保留：它可能是老版本 ffprobe 认不出的合法封装，
// 「删不掉的视频」比「多一个占空间的文件」安全得多。
func pickPruneAction(src string, srcSize int64, outs []probeCandidate) (keep string, drop []string, reason string) {
	valid := make([]int, 0, len(outs))
	for i, o := range outs {
		if o.checked && !o.ok {
			continue // 校验不通过的产物不能顶掉原片
		}
		valid = append(valid, i)
	}
	if len(valid) == 0 {
		return "", nil, "无可用产物（未通过 ffprobe 校验）"
	}
	// 多个有效产物取最小的那个作判定基准（最可能是转码结果）
	idx := valid[0]
	for _, i := range valid {
		if outs[i].size < outs[idx].size {
			idx = i
		}
	}
	best := outs[idx]
	if best.size < int64(float64(srcSize)*postMinOutputRatio) {
		// 产物过小（疑似半成品）：即使校验说它是视频也先按可疑处理，两边都不删
		return "", nil, "产物过小（疑似半成品），保留原片且不删产物"
	}
	if best.size < srcSize {
		// 产物更小且可用 → 原片才是"大"的那份，删原片留产物
		return best.path, []string{src}, "产物更小且可用，删除原片保留产物"
	}
	// 产物更大：删掉校验通过的那些（无效的一个都不删），留下原片
	var others []string
	for _, i := range valid {
		others = append(others, outs[i].path)
	}
	return src, others, "产物更大且可用，保留原片删除产物"
}

// postPrune 转码成功后的大小二选一清理（删除前过一遍 ffprobe 校验）。
//
//	产物 < 源文件 → 删除源文件，保留产物（产物更小说明转码有效，原片体积大是没压过的）
//	产物 >= 源文件 → 保留源文件，删除产物
//
// 但删除源文件是**有前提**的：必须先拿到一个 ffprobe 校验通过的产物，证明它真的
// 解得出视频流。转码命令返回 0 不代表产物可用（转码被中断、封装损坏都会留个小文件），
// 光按大小判定就会把完好的原片换成一个坏文件——这正是这个校验要挡住的情况。
//
// 判定依据是「源文件同目录下、命令开始之后新出现的视频文件」，找不到产物就什么都不动。
// 每次删除都会写一条 pruned 审计记录（不可回退），日志里也留完整路径。
func (m *Manager) postPrune(t models.QBTorrent, src string, started time.Time, pc models.PostProcessConfig) {
	si, err := os.Stat(src)
	if err != nil {
		logger.Warn.Printf("后处理 [%s] 清理前读取源文件失败：%v，跳过清理", t.Name, err)
		return
	}
	dir := filepath.Dir(src)
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Warn.Printf("后处理 [%s] 清理前读取目录 %s 失败：%v，跳过清理", t.Name, dir, err)
		return
	}
	var raw []os.DirEntry
	for _, e := range entries {
		if e.Name()[0] == '.' {
			continue
		}
		if e.Name() == filepath.Base(src) {
			continue // 源文件本身不是产物
		}
		// 扩展名比较要去点：filepath.Ext 返回 ".mov"，表里存的是无点的 "mov"
		ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(e.Name()), "."))
		if !postVideoExts[ext] {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.IsDir() || fi.Size() <= 0 {
			continue
		}
		// 只认「不早于命令开始时刻」的新文件。
		// 这里刻意不加宽容时间：粗粒度文件系统（1s 刻度）只会把新文件的 mtime
		// 向下取整、看起来更旧，于是恰好被滤掉 → 判定为「无产物」→ 不动任何文件，
		// 是安全的方向；反过来放宽时间反而会把早已存在的旧文件当成产物，
		// 进而误删完好的原片。
		if fi.ModTime().Before(started) {
			continue
		}
		raw = append(raw, e)
	}
	if len(raw) == 0 {
		logger.Info.Printf("后处理 [%s] 未在 %s 发现新产物，跳过大小清理", t.Name, dir)
		return
	}

	// 校验器：Verify 打开（默认）时必须能拿到 ffprobe，否则一律放弃删除——
	// 没有校验能力还去删原片，等于把这道闸门拆了。
	var bin string
	if pc.Verify == nil || *pc.Verify {
		bin, _ = findProbeBinary(pc.ProbePath)
		if bin == "" {
			logger.Warn.Printf("后处理 [%s] 已开启「删除前校验」但找不到 ffprobe（可在设置页指定 probePath），"+
				"放弃对 %.1f MB 原片的清理；安装 ffmpeg 或关闭该选项",
				t.Name, float64(si.Size())/1e6)
			return
		}
	}

	var cands []probeCandidate
	for _, e := range raw {
		p := filepath.Join(dir, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}
		// ok 默认 true + checked 标记：没跑校验时视作有效，
		// 这样「关闭删除前校验」时仍能回到纯大小判定，而不是干脆不清理
		c := probeCandidate{path: p, size: fi.Size(), ok: true}
		if bin != "" {
			c.ok, c.detail = probeVideoFile(bin, p)
			c.checked = true
			if c.ok {
				logger.Info.Printf("后处理 [%s] 产物校验通过：%s（%.1f MB，%s）",
					t.Name, p, float64(c.size)/1e6, c.detail)
			} else {
				logger.Warn.Printf("后处理 [%s] 产物校验未通过，不能用于替换原片：%s（%.1f MB）%s",
					t.Name, p, float64(c.size)/1e6, c.detail)
			}
		}
		cands = append(cands, c)
	}

	keep, drop, reason := pickPruneAction(src, si.Size(), cands)
	logger.Info.Printf("后处理 [%s] 大小清理：原片 %.1f MB → %s", t.Name, float64(si.Size())/1e6, reason)
	if len(drop) == 0 {
		// 两种「一个都不删」：产物全没通过校验、或产物小到像半成品。
		// 转码失败留下的坏产物绝不能顶掉原片的位置。
		logger.Warn.Printf("后处理 [%s] 源文件保留不动：%s", t.Name, reason)
		return
	}
	sort.Strings(drop)
	for _, p := range drop {
		if p == keep {
			continue
		}
		m.deletePostFile(t, p)
	}
}

// deletePostFile 删除一个后处理产物/原片并写审计记录（不可回退）。
// 失败只记日志，绝不因此影响其它操作。
func (m *Manager) deletePostFile(t models.QBTorrent, p string) {
	if err := os.Remove(p); err != nil {
		logger.Warn.Printf("后处理 [%s] 删除 %s 失败：%v", t.Name, p, err)
		return
	}
	logger.Info.Printf("后处理 [%s] 已删除 %s", t.Name, p)
	m.audit.Append(auditEntry{
		Hash:    t.Hash,
		Torrent: t.Name,
		Old:     p,
		New:     "（已删除，按大小二选一）",
		Via:     auditViaPrune,
		Status:  AuditPruned,
	})
}

// matchExt 判断文件是否命中扩展名白名单（白名单为空即放行）
func matchExt(file, list string) bool {
	list = strings.ToLower(strings.TrimSpace(list))
	if list == "" {
		return true
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(file), "."))
	for _, e := range strings.Split(list, ",") {
		if e = strings.TrimSpace(e); e != "" && e == ext {
			return true
		}
	}
	return false
}

// RunPostProcessTest 手动跑一次后处理命令（设置页「测试运行」用），同步执行。
// 返回退出码与输出尾部（只保留最后 4KB），不执行任何清理动作——
// 测试的目的是验证命令本身能不能跑通，不该顺手删文件。
func (m *Manager) RunPostProcessTest(file string) (int, string, error) {
	pc := m.cfg.Get().FileManager.PostProcess
	if !pc.Enabled {
		return -1, "", fmt.Errorf("后处理未启用")
	}
	if strings.TrimSpace(pc.Command) == "" {
		return -1, "", fmt.Errorf("请先填写后处理命令")
	}
	if strings.TrimSpace(file) == "" {
		return -1, "", fmt.Errorf("请填写测试用的文件绝对路径")
	}
	t := models.QBTorrent{Name: "（测试）", Category: "", SavePath: filepath.Dir(file)}
	cmdStr, err := renderPostCommand(pc.Command, t, file)
	if err != nil {
		return -1, "", err
	}
	timeout := pc.Timeout
	if timeout <= 0 {
		timeout = int(postTestTimeout.Seconds())
	}
	code, output, err := execPostCommand(cmdStr, t, file, timeout)
	tail := output
	if len(tail) > 4096 {
		tail = "...(已截断)\n" + tail[len(tail)-4096:]
	}
	// 启动失败（不是命令自身失败）时 output 往往为空，光看退出码 -1 完全无从排查，
	// 这里把真实错误落日志：最常见的是 chdir 失败或进程被沙箱/TCC 拒绝启动
	if err != nil {
		logger.Warn.Printf("后处理测试运行失败：%v（退出码 %d，工作目录 %s，命令 %s）",
			err, code, t.SavePath, cmdStr)
	}
	return code, tail, err
}
