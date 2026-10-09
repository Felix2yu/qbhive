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
// 触发点是「该任务没有归档在途」+「qB 报出的文件列表里命中白名单的视频文件」。
// 目标一律以 /torrents/files 为准：文件散在共享分类根时（几百个任务的文件挤在同一
// 个目录），只有 qB 知道哪些文件属于本任务，翻目录既猜不准也极易认错别人的文件。

const (
	// postMaxAttempts 单次启动内，一条命令失败后的最大尝试次数（退避重试，重启不重来）
	postMaxAttempts = 3
	// postMaxWaits 白名单文件持续取不到时的最大等待轮数，超过就终结任务：
	// qB 报出的路径长期不在磁盘上说明它不会再变好，留着只会每轮空转一次 API 调用
	postMaxWaits = 20
	// partialFileSuffix libtorrent 给未下完的文件加的尾缀，去掉它才是最终文件名
	partialFileSuffix = ".!qB"
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

// postEnvNames execPostCommand 注入的环境变量名，与 renderPostCommand 的 {} 占位符一一对应
// （值原样交给 shell，由使用者加引号）
var postEnvNames = []string{"FILE", "DIR", "NAME", "CATEGORY", "TAGS", "SAVEPATH"}

// postRetryDelay 每次失败后的重试退避基数（第 n 次失败后等 n×该值），乘上后按
// 剩余超时裁短。用 var 而非 const：单测要把它调小才能秒级跑完重试用例。
var postRetryDelay = 20 * time.Second

// postEnabled 后处理是否已开启
func (m *Manager) postEnabled() bool {
	return m.cfg.Get().FileManager.PostProcess.Enabled
}

// postTaskFiltered 整任务级的分类/标签过滤：返回是否跳过后处理以及写进日志的原因。
// 四个名单全留空即不过滤。判定只用 qB 报出的 category/tags，不发任何请求，所以它排在
// 取文件列表之前——被过滤掉的任务不该为它花掉一次 /torrents/files。
func postTaskFiltered(t models.QBTorrent, pc models.PostProcessConfig) (bool, string) {
	// 名单一律先拆项：只写了逗号空格的「空名单」等价于没配，不能当成白名单把所有任务挡掉
	catInc := splitNameList(pc.CategoryInclude)
	catExc := splitNameList(pc.CategoryExclude)
	tagInc := splitNameList(pc.TagInclude)
	tagExc := splitNameList(pc.TagExclude)

	if nameListHit(t.Category, catExc) {
		return true, fmt.Sprintf("分类 %q 命中排除名单（%s）", strings.TrimSpace(t.Category), strings.Join(catExc, ","))
	}
	if len(catInc) > 0 && !nameListHit(t.Category, catInc) {
		return true, fmt.Sprintf("分类 %q 不在包含名单（%s）", strings.TrimSpace(t.Category), strings.Join(catInc, ","))
	}
	tags := splitNameList(t.Tags)
	for _, tag := range tags {
		if nameListHit(tag, tagExc) {
			return true, fmt.Sprintf("标签 %q 命中排除名单（%s）", tag, strings.Join(tagExc, ","))
		}
	}
	if len(tagInc) > 0 && !anyNameListHit(tags, tagInc) {
		return true, fmt.Sprintf("标签 %q 不在包含名单（%s）", strings.Join(tags, ","), strings.Join(tagInc, ","))
	}
	return false, ""
}

// ensurePostFilterRescan 过滤名单一改就清空进程内的「已跳过」表（仅 scan goroutine 调用）。
// m.done 一旦记下某个任务，这一进程里就不会再回头看它；不清空就会出现「放宽了名单、
// 历史任务却纹丝不动」，只能靠重启解决。要不要真的重转仍由每个任务落盘的
// PostFilterList 指纹决定：正常转完的任务（指纹为空）不会被动摇，
// 已派发的文件也仍由 PostDispatched 去重，重扫只会补转过滤期间漏掉的那些。
func (m *Manager) ensurePostFilterRescan(pc models.PostProcessConfig) {
	sig := postFilterSignature(pc)
	if sig == m.postSig {
		return
	}
	m.postSig = sig
	if !pc.Enabled || len(m.done) == 0 {
		return
	}
	logger.Info.Printf("转码过滤名单已修改（%s），重新评估 %d 个已跳过的任务", sig, len(m.done))
	m.done = make(map[string]bool)
}

// postFilterSignature 分类/标签名单的指纹，用于判断「用户改过过滤配置没有」。
// 存的是拆分后的原文而不是摘要：这个值要写进状态文件供人回看排查，
// 顺带把「动漫, 电影」和「动漫 ,电影」这类纯空格改动归一成同一个指纹，免得白白重开一批任务。
func postFilterSignature(pc models.PostProcessConfig) string {
	return fmt.Sprintf("ci=%s|ce=%s|ti=%s|te=%s",
		strings.Join(splitNameList(pc.CategoryInclude), ","),
		strings.Join(splitNameList(pc.CategoryExclude), ","),
		strings.Join(splitNameList(pc.TagInclude), ","),
		strings.Join(splitNameList(pc.TagExclude), ","))
}

// markPostFiltered 记下「本任务被过滤挡掉」：终结 + 当时的名单指纹。
// 只置内存标记，落盘交给 flushPostState（一轮扫描一次）
func (m *Manager) markPostFiltered(hash, sig string) {
	m.mu.Lock()
	if rec := m.cleanState[hash]; rec != nil {
		rec.PostDone = true
		rec.PostFilterList = sig
	} else {
		m.cleanState[hash] = &cleanRecord{PostDone: true, PostFilterList: sig}
	}
	m.postDirty = true
	m.mu.Unlock()
}

// reopenPostFiltered 名单改过就把「因过滤而终结」的任务解除终结，返回是否重开。
// 已派发的文件（PostDispatched）原样保留，重开只会补转过滤期间漏掉的文件，不会重转。
func (m *Manager) reopenPostFiltered(hash, sig string) bool {
	m.mu.Lock()
	rec := m.cleanState[hash]
	reopened := rec != nil && rec.PostDone && rec.PostFilterList != "" && rec.PostFilterList != sig
	if reopened {
		rec.PostDone = false
		rec.PostFilterList = ""
		rec.PostWaits = 0
		m.postDirty = true
	}
	m.mu.Unlock()
	return reopened
}

// flushPostState 收尾一轮扫描：把攒下的过滤状态变更一次性落盘，并汇总本轮被过滤挡掉的任务数
func (m *Manager) flushPostState() {
	m.mu.Lock()
	dirty := m.postDirty
	m.postDirty = false
	m.mu.Unlock()
	if dirty {
		m.saveCleanState()
	}
	if m.postSkipped > 0 {
		logger.Info.Printf("转码过滤本轮挡掉 %d 个任务（名单：%s），原因见调试日志", m.postSkipped, m.postSig)
		m.postSkipped = 0
	}
}

// postListSeparators 名单分隔符：除了半角逗号，全角逗号、顿号、分号（半/全角）、
// 竖线与换行都按分隔符处理。中文分类名用输入法打出来常常带「，」，只认半角逗号
// 会让整串被当成一个匹配不上的分类，看起来就像名单没生效。
// 代价是真的含这些字符的分类/标签名没法列进来——qB 里不会出现这种名字。
var postListSeparators = strings.NewReplacer(
	"，", ",", "、", ",", "；", ",", ";", ",", "｜", ",", "|", ",",
	"\r", ",", "\n", ",", "\t", " ",
)

// splitNameList 把名单拆成去空白、去空项的列表（分隔符见 postListSeparators），
// 保留原大小写用于展示与比对。qB 的 tags 与用户配的名单都走这里。
func splitNameList(list string) []string {
	out := make([]string, 0, 4)
	for _, item := range strings.Split(postListSeparators.Replace(list), ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// nameListHit val 是否命中名单（不区分大小写）。空 val 永远不算命中：未分类、无标签的
// 任务因此不会满足任何包含名单，也不会被排除名单误伤。
func nameListHit(val string, list []string) bool {
	v := strings.ToLower(strings.TrimSpace(val))
	if v == "" {
		return false
	}
	for _, item := range list {
		if strings.ToLower(item) == v {
			return true
		}
	}
	return false
}

// anyNameListHit 任务的一组标签里是否有任意一个命中名单
func anyNameListHit(vals, list []string) bool {
	for _, v := range vals {
		if nameListHit(v, list) {
			return true
		}
	}
	return false
}

// handlePostProcess 推进一个任务的后处理队列：每轮最多派发一个文件，全局串行。
//
// 返回 (done, retry)：
//
//	done  — 本任务已无待办（未开启、已终结、文件列表里没有命中白名单且没派发过的
//	        文件），调用方据此把任务标 done，之后不再进入处理
//	retry — 暂缓（归档还在等 qB 异步移动落地、文件列表取不到、已有转码在跑、
//	        或本任务仍有文件排在队里），调用方先不标 done 以便下轮再试。
//	        命令本身失败时的重试在后台 goroutine 内退避完成，不走这条路径
func (m *Manager) handlePostProcess(t models.QBTorrent) (done bool, retry bool) {
	pc := m.cfg.Get().FileManager.PostProcess
	if !pc.Enabled {
		return true, false
	}
	sig := postFilterSignature(pc)
	if m.postSettled(t.Hash) {
		// 因分类/标签过滤而终结的任务：名单一改就重新判定，让「先过滤错、后放宽名单」
		// 的历史任务补转（已派发的文件仍由 PostDispatched 去重，不会重转一遍）。
		// 这里不逐条打日志：一次改动会重开整批任务，汇总见 ensurePostFilterRescan
		if !m.reopenPostFiltered(t.Hash, sig) {
			return true, false
		}
	}
	// 命令都没配就别占着任务反复重试：终结掉并把原因写清楚
	if strings.TrimSpace(pc.Command) == "" {
		logger.Warn.Printf("后处理 [%s] 已启用但未配置命令，跳过", t.Name)
		m.markPostSettled(t.Hash)
		return true, false
	}
	// 分类/标签过滤：整个任务不参与转码，一次请求都不发。判定结果连同当时的名单
	// 一起持久化，重启不用每轮重判；改名单则由 reopenPostFiltered 解除终结。
	// 逐条只记 Debug（历史库里一次改动可能挡掉几百个任务，全写 Info 会把环形缓冲冲满），
	// 每轮结束由 flushPostState 补一条汇总。
	if skip, why := postTaskFiltered(t, pc); skip {
		logger.Debug.Printf("后处理 [%s] %s，跳过转码", t.Name, why)
		m.postSkipped++
		m.markPostFiltered(t.Hash, sig)
		return true, false
	}
	// 归档还在等待 qB 异步移动落地（原目录未清空）时必须让路：
	// 此时文件还没到最终落点，任何转码都是作用在旧路径上。
	if dir := m.getFlattenDir(t.Hash); dir != "" {
		logger.Debug.Printf("后处理 [%s] 归档待落地（%s），让路等下轮", t.Name, dir)
		return false, true
	}

	targets, waiting, err := m.pendingPostTargets(t, pc)
	if err != nil {
		logger.Warn.Printf("后处理 [%s] 取任务文件列表失败：%v，下轮重试", t.Name, err)
		return false, true
	}
	if len(targets) == 0 {
		// 命中白名单的文件还在 qB 的临时名里（尾缀 !qB）或磁盘上取不到：
		// 这是暂时状态，先让路；连续多轮取不到才终结，否则等文件落齐了也再没人来转码
		if waiting && !m.postWaitExhausted(t.Hash) {
			logger.Debug.Printf("后处理 [%s] 待转文件尚未落齐，下轮再看", t.Name)
			return false, true
		}
		m.markPostSettled(t.Hash)
		return true, false
	}
	// 全局串行闸门：转码吃满 CPU 与磁盘带宽，两条命令一起跑只会都变慢，
	// 在 macOS 上还会互相抢同一个转码工具
	if !m.tryAcquirePost() {
		logger.Debug.Printf("后处理 [%s] 已有转码在跑，%d 个文件待派发，下轮再看", t.Name, len(targets))
		return false, true
	}
	file := targets[0]
	logger.Info.Printf("后处理 [%s] 派发转码：%s（本任务还有 %d 个文件待处理）", t.Name, file, len(targets)-1)
	// 派发即落盘：命令可能跑几十分钟，中途重启不该对同一个文件再转一遍。
	// 命令失败由 runPostCommandWithRetry 的退避重试兜底。
	m.markPostDispatched(t.Hash, file)
	go func() {
		defer m.releasePost()
		m.runPostProcess(t, file, pc)
	}()
	// 本轮不终结：剩下的文件还要排队，等它们全派发完（或确认无需转码）再终结
	return false, false
}

// postSettled 该任务的后处理是否已终结
func (m *Manager) postSettled(hash string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.cleanState[hash]
	return rec != nil && rec.PostDone
}

// markPostSettled 标记任务后处理终结并落盘（重启后不再重复解析）
func (m *Manager) markPostSettled(hash string) {
	m.mu.Lock()
	if rec := m.cleanState[hash]; rec != nil {
		rec.PostDone = true
		// 清掉过滤指纹：这是「转完了/确实没有可转文件」的正常终结，改名单不该把它重开
		rec.PostFilterList = ""
	} else {
		m.cleanState[hash] = &cleanRecord{PostDone: true}
	}
	m.mu.Unlock()
	m.saveCleanState()
}

// markPostDispatched 记录「已交给 shell 的文件」并落盘，作为文件级的派发去重
func (m *Manager) markPostDispatched(hash, file string) {
	m.mu.Lock()
	rec := m.cleanState[hash]
	if rec == nil {
		rec = &cleanRecord{}
		m.cleanState[hash] = rec
	}
	rec.PostDispatched = append(rec.PostDispatched, file)
	rec.PostWaits = 0
	m.mu.Unlock()
	m.saveCleanState()
}

// postWaitExhausted 待转文件连续多少轮都取不到；到上限才允许终结任务。
// 只计数不落盘（下一轮还会再看），落盘的是最终那次 markPostSettled。
func (m *Manager) postWaitExhausted(hash string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.cleanState[hash]
	if rec == nil {
		rec = &cleanRecord{}
		m.cleanState[hash] = rec
	}
	rec.PostWaits++
	return rec.PostWaits >= postMaxWaits
}

// tryAcquirePost / releasePost 全局串行闸门（一次只允许一条后处理命令在跑）。
// 闸门在派发前同步占用、后台命令结束时释放，所以扫描轮询之间也不会出现两条并发。
func (m *Manager) tryAcquirePost() bool {
	m.postRunMu.Lock()
	defer m.postRunMu.Unlock()
	if m.postBusy {
		return false
	}
	m.postBusy = true
	return true
}

func (m *Manager) releasePost() {
	m.postRunMu.Lock()
	m.postBusy = false
	m.postRunMu.Unlock()
}

// pendingPostTargets 本任务尚未派发过、且命中白名单的目标文件（保持 qB 的文件顺序）。
// waiting 表示「有该转的文件但这一轮还拿不到」——qB 仍用临时名 !qB 写着、或磁盘上
// 暂时 stat 不到，调用方据此暂缓而不是把任务终结掉。
// 取不到文件列表时把错误原样抛给调用方：那是临时错误，绝不能被当成「没有要转的文件」。
func (m *Manager) pendingPostTargets(t models.QBTorrent, pc models.PostProcessConfig) (targets []string, waiting bool, err error) {
	files, err := m.client.GetTorrentFiles(t.Hash)
	if err != nil {
		return nil, false, err
	}
	m.mu.Lock()
	var dispatched []string
	if rec := m.cleanState[t.Hash]; rec != nil {
		dispatched = append(dispatched, rec.PostDispatched...)
	}
	m.mu.Unlock()
	done := make(map[string]bool, len(dispatched))
	for _, p := range dispatched {
		done[p] = true
	}

	out := make([]string, 0, len(files))
	matched := 0
	for _, f := range files {
		name := filepath.ToSlash(f.Name)
		partial := strings.HasSuffix(name, partialFileSuffix)
		name = strings.TrimSuffix(name, partialFileSuffix)
		if !matchExt(name, pc.Extensions) {
			continue
		}
		matched++
		p, ok := postTargetPath(t.SavePath, name)
		if !ok {
			continue // 文件名越界：永久忽略，不参与等待
		}
		if partial {
			// qB 还在用未完成临时名写着这个文件，转它等于转半个片子
			waiting = true
			continue
		}
		if fi, err := os.Stat(p); err != nil || !fi.Mode().IsRegular() || fi.Size() <= 0 {
			waiting = true // 还没落盘或已被移走：留给下轮
			continue
		}
		if done[p] {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 && matched > 0 {
		logger.Debug.Printf("后处理 [%s] 文件列表 %d 项、命中白名单 %d 项，无待派发文件（%q）",
			t.Name, len(files), matched, pc.Extensions)
	}
	return out, waiting, nil
}

// postTargetPath 把 qB 报告的文件内部路径（相对 save_path、用正斜杠）换成磁盘绝对路径。
// 种子可以自带上跳的文件名（"../../x.mp4"），拼完必须仍在 save_path 之内，
// 否则忽略——命令拿到的是用户自己配的 shell 模板，绝不能让它顺着一个越界路径跑出去。
func postTargetPath(savePath, name string) (string, bool) {
	if savePath == "" || name == "" {
		return "", false
	}
	p := filepath.Join(savePath, filepath.FromSlash(name))
	rel, err := filepath.Rel(savePath, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		logger.Warn.Printf("后处理：任务文件 %q 落在保存路径 %s 之外，忽略", name, savePath)
		return "", false
	}
	return p, true
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
		{"{tags}", t.Tags},
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
		"TAGS="+t.Tags,
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
// 判定依据是「源文件同目录下、与原片同名（只换扩展名）、且不早于命令开始时刻的新
// 视频文件」，三条同时满足才算产物，找不到就什么都不动。
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
		// 产物必须与原片同名（只换扩展名）才算数。文件散在共享分类根时，
		// 「同目录 + mtime 不早于开始」完全不能证明这个视频是本次转码的产物——
		// 它很可能是另一个任务刚下完的片子，被认错就会拿别人的文件去比大小、
		// 甚至把别人的文件当产物删掉。
		if strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())) !=
			strings.TrimSuffix(filepath.Base(src), filepath.Ext(src)) {
			continue
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

// matchExt 判断文件是否命中扩展名白名单（白名单为空即放行）。
// 分隔符与分类/标签名单同一套（见 postListSeparators），全角逗号也算分隔
func matchExt(file, list string) bool {
	allowed := splitNameList(strings.ToLower(list))
	if len(allowed) == 0 {
		return true
	}
	return nameListHit(strings.TrimPrefix(filepath.Ext(file), "."), allowed)
}

// PostTestResult 设置页「测试运行」的结果：命令退出码、输出尾部，
// 以及按当前名单这一组分类/标签到底会不会真的被转码。
type PostTestResult struct {
	Code   int    `json:"exitCode"`
	Output string `json:"output"`
	// Filtered 按已保存的分类/标签名单判定为「跳过转码」。注意判定只是回报，
	// 本次测试命令照样会跑——测试运行是用来验证命令的，不该被名单挡住
	Filtered bool   `json:"filtered"`
	Reason   string `json:"reason,omitempty"`
}

// RunPostProcessTest 手动跑一次后处理命令（设置页「测试运行」用），同步执行。
// category/tags 是模拟的任务上下文：填了就能带着 $CATEGORY / $TAGS（以及 {category}
// {tags} 占位符）验证脚本按分类/标签分派的分支，不填就是空值。
// 不执行任何清理动作——测试的目的是验证命令本身能不能跑通，不该顺手删文件。
func (m *Manager) RunPostProcessTest(file, category, tags string) (*PostTestResult, error) {
	pc := m.cfg.Get().FileManager.PostProcess
	if !pc.Enabled {
		return &PostTestResult{Code: -1}, fmt.Errorf("后处理未启用")
	}
	if strings.TrimSpace(pc.Command) == "" {
		return &PostTestResult{Code: -1}, fmt.Errorf("请先填写后处理命令")
	}
	if strings.TrimSpace(file) == "" {
		return &PostTestResult{Code: -1}, fmt.Errorf("请填写测试用的文件绝对路径")
	}
	t := models.QBTorrent{Name: "（测试）", Category: category, Tags: tags, SavePath: filepath.Dir(file)}
	skip, why := postTaskFiltered(t, pc)
	res := &PostTestResult{Filtered: skip, Reason: why}
	cmdStr, err := renderPostCommand(pc.Command, t, file)
	if err != nil {
		res.Code = -1
		return res, err
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
	res.Code, res.Output = code, tail
	// 启动失败（不是命令自身失败）时 output 往往为空，光看退出码 -1 完全无从排查，
	// 这里把真实错误落日志：最常见的是 chdir 失败或进程被沙箱/TCC 拒绝启动
	if err != nil {
		logger.Warn.Printf("后处理测试运行失败：%v（退出码 %d，工作目录 %s，命令 %s）",
			err, code, t.SavePath, cmdStr)
	}
	return res, err
}
