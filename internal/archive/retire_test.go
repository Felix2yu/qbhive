package archive

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// fakeFileState 直接摆出文件侧收敛状态，不必把清理/转码流水线跑一遍
type fakeFileState struct {
	notSettled map[string]bool
	notIdle    map[string]bool
	gotQuiet   time.Duration
}

func (f *fakeFileState) Settled(hash string, quiet time.Duration) bool {
	f.gotQuiet = quiet
	return !f.notSettled[hash]
}

func (f *fakeFileState) Idle(hash string) bool { return !f.notIdle[hash] }

type deleteCall struct {
	hashes []string
	files  bool
}

type retireEnv struct {
	r     *Retirer
	store *Store
	cfg   *config.Manager
	files *fakeFileState
	srv   *httptest.Server

	mu       sync.Mutex
	deletes  []deleteCall
	infoN    int
	infoList string
	maindata string
	delCode  int
	notices  []string
}

func (e *retireEnv) deleteCalls() []deleteCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]deleteCall(nil), e.deletes...)
}

func (e *retireEnv) infoCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.infoN
}

func (e *retireEnv) setInfo(list string) {
	e.mu.Lock()
	e.infoList = list
	e.mu.Unlock()
}

func (e *retireEnv) setMaindata(body string) {
	e.mu.Lock()
	e.maindata = body
	e.mu.Unlock()
}

func (e *retireEnv) notified() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.notices...)
}

// archiveCfg 一份「门槛全关、只退失去实体」的起步配置：
// 与老配置升级后的实际状态一致，测健康路径时再显式打开门槛
func archiveCfg() models.ArchiveConfig {
	return models.ArchiveConfig{
		Enabled: true, Interval: 60, BatchSize: 100, QuietMinutes: 5,
		DryRun: boolPtr(false), ExemptLostFiles: boolPtr(true),
		Guard: models.GuardConfig{Mode: models.GuardModeOff},
	}
}

func boolPtr(b bool) *bool { return &b }

func newRetireEnv(t *testing.T, ac models.ArchiveConfig, torrents ...models.QBTorrent) *retireEnv {
	t.Helper()
	dir := t.TempDir()
	e := &retireEnv{
		files:    &fakeFileState{notSettled: map[string]bool{}, notIdle: map[string]bool{}},
		infoList: torrentsJSON(t, torrents),
		delCode:  http.StatusOK,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		e.infoN++
		body := e.infoList
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v2/sync/maindata", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		body := e.maindata
		e.mu.Unlock()
		if body == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})
	mux.HandleFunc("/api/v2/torrents/delete", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		e.deletes = append(e.deletes, deleteCall{
			hashes: strings.Split(r.PostFormValue("hashes"), "|"),
			files:  r.PostFormValue("deleteFiles") == "true",
		})
		code := e.delCode
		e.mu.Unlock()
		w.WriteHeader(code)
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)

	e.cfg = config.New(filepath.Join(dir, "config.json"))
	cfg := e.cfg.Get()
	cfg.Archive = ac
	e.cfg.Set(cfg)

	e.store = NewStore(filepath.Join(dir, "archive.jsonl"))
	e.r = NewRetirer(e.cfg, qb.New(e.srv.URL, "u", "p", ""), e.store, e.files)
	e.r.SetNotifier(func(title, body string) {
		e.mu.Lock()
		e.notices = append(e.notices, title+"|"+body)
		e.mu.Unlock()
	})
	return e
}

func torrentsJSON(t *testing.T, list []models.QBTorrent) string {
	t.Helper()
	var b strings.Builder
	b.WriteByte('[')
	for i, tor := range list {
		if i > 0 {
			b.WriteByte(',')
		}
		raw, err := json.Marshal(tor)
		if err != nil {
			t.Fatalf("序列化任务: %v", err)
		}
		b.Write(raw)
	}
	b.WriteByte(']')
	return b.String()
}

// lostTorrent 一条失去实体的空壳：原片已被转码清理
func lostTorrent(hash string) models.QBTorrent {
	return models.QBTorrent{
		Hash: hash, Name: "影片." + hash, State: "missingFiles", Progress: 1,
		Size: 1000, Uploaded: 2000, AddedOn: 1000, CompletedOn: 2000, Ratio: 2,
		SavePath: "/data/电影", ContentPath: "/data/电影/影片." + hash + ".mkv",
		Category: "电影", Tags: "转码", Tracker: "http://tr.example.com/announce",
	}
}

// healthyTorrent 一条健康完成、正在做种的任务
func healthyTorrent(hash string) models.QBTorrent {
	t := lostTorrent(hash)
	t.State = "stoppedUP"
	return t
}

func TestRetirer_LostFilesExemptFromGates(t *testing.T) {
	ac := archiveCfg() // 三个门槛都是 0
	e := newRetireEnv(t, ac, lostTorrent("a"))
	e.r.scan()

	calls := e.deleteCalls()
	if len(calls) != 1 || len(calls[0].hashes) != 1 || calls[0].hashes[0] != "a" {
		t.Fatalf("应移除 a，got %+v", calls)
	}
	if calls[0].files {
		t.Error("deleteFiles 必须恒为 false：qB 会按元数据路径删掉完好原片")
	}
	rec, ok := e.store.Get("a")
	if !ok {
		t.Fatal("应先建档")
	}
	if rec.Reason != ReasonLostFiles {
		t.Errorf("理由应为 %s，got %s", ReasonLostFiles, rec.Reason)
	}
	if rec.RetiredAt == 0 {
		t.Error("移除成功后应回填 RetiredAt")
	}
}

// CompletedOn==0 的失去实体任务退不掉：无法构造有意义的记录，
// 也可能是正在下载却暂时读不到文件的任务
func TestRetirer_LostFilesRequiresCompletedOn(t *testing.T) {
	tor := lostTorrent("a")
	tor.CompletedOn = 0
	e := newRetireEnv(t, archiveCfg(), tor)
	e.r.scan()
	if len(e.deleteCalls()) != 0 {
		t.Error("没有完成时间的空壳不该退休")
	}
	if e.store.Len() != 0 {
		t.Error("不该建档")
	}
}

func TestRetirer_LostFilesNotExemptUsesGates(t *testing.T) {
	ac := archiveCfg()
	ac.ExemptLostFiles = boolPtr(false)
	e := newRetireEnv(t, ac, lostTorrent("a"))
	e.r.scan()
	if len(e.deleteCalls()) != 0 {
		t.Error("关掉豁免后，门槛全 0 时失去实体任务也不该退休")
	}

	ac.MinRatio = 1.0
	e2 := newRetireEnv(t, ac, lostTorrent("a"))
	e2.r.scan()
	rec, ok := e2.store.Get("a")
	if !ok || rec.Reason != ReasonRatio {
		t.Fatalf("过门槛后应按比率退休，got %+v ok=%v", rec, ok)
	}
}

func TestRetirer_HealthyRequiresGate(t *testing.T) {
	e := newRetireEnv(t, archiveCfg(), healthyTorrent("a"))
	e.r.scan()
	if len(e.deleteCalls()) != 0 || e.store.Len() != 0 {
		t.Error("门槛全 0 时健康完成任务一条都不该退（老配置升级后的行为）")
	}
}

func TestRetirer_SeedingGates(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name   string
		ac     func(*models.ArchiveConfig)
		tor    func(*models.QBTorrent)
		want   string
		retire bool
	}{
		{"比率达标", func(a *models.ArchiveConfig) { a.MinRatio = 1.0 },
			func(t *models.QBTorrent) { t.Ratio = 1.0 }, ReasonRatio, true},
		{"比率差一点", func(a *models.ArchiveConfig) { a.MinRatio = 1.0 },
			func(t *models.QBTorrent) { t.Ratio = 0.99 }, "", false},
		{"时长达标", func(a *models.ArchiveConfig) { a.MinHours = 24 },
			func(t *models.QBTorrent) { t.Ratio = 0; t.CompletedOn = now - 24*3600 }, ReasonHours, true},
		{"时长差一秒", func(a *models.ArchiveConfig) { a.MinHours = 24 },
			func(t *models.QBTorrent) { t.Ratio = 0; t.CompletedOn = now - 24*3600 + 1 }, "", false},
		{"上传量达标", func(a *models.ArchiveConfig) { a.MinUploadedMB = 500 },
			func(t *models.QBTorrent) { t.Ratio = 0; t.CompletedOn = now; t.Uploaded = 500 * 1024 * 1024 }, ReasonUploaded, true},
		{"上传量差一字节", func(a *models.ArchiveConfig) { a.MinUploadedMB = 500 },
			func(t *models.QBTorrent) { t.Ratio = 0; t.CompletedOn = now; t.Uploaded = 500*1024*1024 - 1 }, "", false},
		{"三者任一即可", func(a *models.ArchiveConfig) { a.MinRatio = 99; a.MinHours = 9999; a.MinUploadedMB = 500 },
			func(t *models.QBTorrent) { t.Ratio = 0; t.CompletedOn = now; t.Uploaded = 600 * 1024 * 1024 }, ReasonUploaded, true},
		{"全不达标", func(a *models.ArchiveConfig) { a.MinRatio = 99; a.MinHours = 9999; a.MinUploadedMB = 99999 },
			func(t *models.QBTorrent) { t.Ratio = 1; t.CompletedOn = now; t.Uploaded = 1024 }, "", false},
		{"门槛为0不启用", func(a *models.ArchiveConfig) { a.MinRatio = 0 },
			func(t *models.QBTorrent) { t.Ratio = 100 }, "", false},
	}
	for _, c := range cases {
		ac := archiveCfg()
		c.ac(&ac)
		tor := healthyTorrent("a")
		c.tor(&tor)
		e := newRetireEnv(t, ac, tor)
		e.r.scan()
		rec, ok := e.store.Get("a")
		if c.retire {
			if !ok || rec.Reason != c.want {
				t.Errorf("%s: 应退休且理由为 %s，got %+v ok=%v", c.name, c.want, rec, ok)
			}
			if len(e.deleteCalls()) != 1 {
				t.Errorf("%s: 应发出一次删除", c.name)
			}
		} else if ok || len(e.deleteCalls()) != 0 {
			t.Errorf("%s: 不该退休，got %+v ok=%v deletes=%d", c.name, rec, ok, len(e.deleteCalls()))
		}
	}
}

func TestRetirer_StateWhitelist(t *testing.T) {
	ac := archiveCfg()
	ac.MinRatio = 1.0
	for _, state := range []string{"stoppedUP", "stalledUP", "uploading", "queuedUP", "forcedUP"} {
		tor := healthyTorrent("a")
		tor.State = state
		e := newRetireEnv(t, ac, tor)
		e.r.scan()
		if len(e.deleteCalls()) != 1 {
			t.Errorf("%s 属于完成做种态，应退休", state)
		}
	}
	// checkingUP 校验中磁盘状态不可信；error 且无完成时间不算失去实体
	for _, state := range []string{"downloading", "stoppedDL", "stalledDL", "checkingDL", "checkingUP", "checkingResumeData", "moving", "pausedUP", "error"} {
		tor := healthyTorrent("a")
		tor.State = state
		tor.CompletedOn = 0
		e := newRetireEnv(t, ac, tor)
		e.r.scan()
		if len(e.deleteCalls()) != 0 {
			t.Errorf("%s 不该退休", state)
		}
	}
}

func TestRetirer_ProgressThreshold(t *testing.T) {
	ac := archiveCfg()
	ac.MinRatio = 1.0
	for _, p := range []float64{0, 0.5, 0.99, 0.9989} {
		tor := healthyTorrent("a")
		tor.Progress = p
		e := newRetireEnv(t, ac, tor)
		e.r.scan()
		if len(e.deleteCalls()) != 0 {
			t.Errorf("progress=%v 未达完成阈值，不该退休", p)
		}
	}
	tor := healthyTorrent("a")
	tor.Progress = 0.999
	e := newRetireEnv(t, ac, tor)
	e.r.scan()
	if len(e.deleteCalls()) != 1 {
		t.Error("progress=0.999 应视为完成")
	}
}

func TestRetirer_NameLists(t *testing.T) {
	cases := []struct {
		name   string
		catInc string
		catExc string
		tagInc string
		tagExc string
		retire bool
	}{
		{"全空不过滤", "", "", "", "", true},
		{"分类在包含名单", "电影,动漫", "", "", "", true},
		{"分类不在包含名单", "动漫", "", "", "", false},
		{"分类命中排除", "", "电影", "", "", false},
		{"标签在包含名单", "", "", "转码", "", true},
		{"标签不在包含名单", "", "", "4k", "", false},
		{"标签命中排除", "", "", "", "转码", false},
	}
	for _, c := range cases {
		ac := archiveCfg()
		ac.CategoryInclude, ac.CategoryExclude = c.catInc, c.catExc
		ac.TagInclude, ac.TagExclude = c.tagInc, c.tagExc
		e := newRetireEnv(t, ac, lostTorrent("a"))
		e.r.scan()
		if got := len(e.deleteCalls()) == 1; got != c.retire {
			t.Errorf("%s: retire=%v want=%v", c.name, got, c.retire)
		}
	}
}

func TestRetirer_FileStateBlocks(t *testing.T) {
	ac := archiveCfg()
	ac.MinRatio = 1.0
	ac.QuietMinutes = 7

	e := newRetireEnv(t, ac, healthyTorrent("a"))
	e.files.notSettled["a"] = true
	e.r.scan()
	if len(e.deleteCalls()) != 0 {
		t.Error("健康任务文件侧未收敛时不该退休")
	}
	if e.files.gotQuiet != 7*time.Minute {
		t.Errorf("静默期应传 7m，got %s", e.files.gotQuiet)
	}

	e2 := newRetireEnv(t, archiveCfg(), lostTorrent("b"))
	e2.files.notIdle["b"] = true
	e2.r.scan()
	if len(e2.deleteCalls()) != 0 {
		t.Error("失去实体但仍有在跑的转码命令时不该退休")
	}

	// 失去实体走 Idle 而不是 Settled：它多半根本没有 filemgr 记录
	e3 := newRetireEnv(t, archiveCfg(), lostTorrent("c"))
	e3.files.notSettled["c"] = true
	e3.r.scan()
	if len(e3.deleteCalls()) != 1 {
		t.Error("失去实体任务不该被 Settled 拦住（否则 7155 条一条都退不掉）")
	}
}

func TestRetirer_DryRunSendsNoDelete(t *testing.T) {
	ac := archiveCfg()
	ac.DryRun = boolPtr(true)
	e := newRetireEnv(t, ac, lostTorrent("a"), lostTorrent("b"))
	e.r.scan()

	if len(e.deleteCalls()) != 0 {
		t.Fatalf("干跑绝不能发删除请求，got %+v", e.deleteCalls())
	}
	if e.store.Len() != 2 {
		t.Fatalf("干跑应建档 2 条，got %d", e.store.Len())
	}
	for _, h := range []string{"a", "b"} {
		if rec, _ := e.store.Get(h); rec.RetiredAt != 0 {
			t.Errorf("%s 干跑候选的 RetiredAt 应为 0", h)
		}
	}
	if len(e.notified()) != 1 {
		t.Errorf("干跑发现候选应发一条通知，got %v", e.notified())
	}

	// 关掉干跑后同一批候选直接进删除批次，不重复建档
	ac.DryRun = boolPtr(false)
	cfg := e.cfg.Get()
	cfg.Archive = ac
	e.cfg.Set(cfg)
	e.r.scan()
	if len(e.deleteCalls()) != 1 || len(e.deleteCalls()[0].hashes) != 2 {
		t.Fatalf("关干跑后应一批移除 2 条，got %+v", e.deleteCalls())
	}
}

// 账本写失败必须零删除请求：这是「先建档后删」的核心闸门。
// 把 scan 里 Append 失败后的 continue 去掉，这条测试就会红。
func TestRetirer_StoreFailureSendsNoDelete(t *testing.T) {
	dir := t.TempDir()
	e := newRetireEnv(t, archiveCfg(), lostTorrent("a"))
	// 把账本路径换成一个已存在的目录：Append 必然失败
	e.store = NewStore(dir)
	e.r.store = e.store

	e.r.scan()
	if len(e.deleteCalls()) != 0 {
		t.Fatalf("建档失败时绝不能删任务，got %+v", e.deleteCalls())
	}
	if e.store.Len() != 0 {
		t.Error("写失败时内存状态不该更新")
	}
}

func TestRetirer_BatchSize(t *testing.T) {
	ac := archiveCfg()
	ac.BatchSize = 2
	var list []models.QBTorrent
	for _, h := range []string{"a", "b", "c", "d", "e"} {
		list = append(list, lostTorrent(h))
	}
	e := newRetireEnv(t, ac, list...)

	e.r.scan()
	calls := e.deleteCalls()
	if len(calls) != 1 || len(calls[0].hashes) != 2 {
		t.Fatalf("单轮应只提交一批 2 条，got %+v", calls)
	}
	if e.store.Len() != 2 {
		t.Errorf("本轮只处理到前 2 条，got %d", e.store.Len())
	}
	e.r.scan()
	e.r.scan()
	if got := len(e.deleteCalls()); got != 3 {
		t.Fatalf("三轮应排空 5 条（2+2+1），got %d 批", got)
	}
	for _, h := range []string{"a", "b", "c", "d", "e"} {
		if rec, _ := e.store.Get(h); rec.RetiredAt == 0 {
			t.Errorf("%s 应已回填退休时刻", h)
		}
	}
}

func TestRetirer_DeleteFailureKeepsCandidate(t *testing.T) {
	e := newRetireEnv(t, archiveCfg(), lostTorrent("a"))
	e.mu.Lock()
	e.delCode = http.StatusInternalServerError
	e.mu.Unlock()

	e.r.scan()
	rec, ok := e.store.Get("a")
	if !ok {
		t.Fatal("删失败也应保留账本记录")
	}
	if rec.RetiredAt != 0 {
		t.Error("删失败不该回填退休时刻")
	}

	e.mu.Lock()
	e.delCode = http.StatusOK
	e.mu.Unlock()
	e.r.scan()
	if rec, _ := e.store.Get("a"); rec.RetiredAt == 0 {
		t.Error("下轮重试成功后应回填退休时刻")
	}
}

// 已退休但快照还残留着这条（快照最多旧 2s）：不能重复删
func TestRetirer_SkipsAlreadyRetired(t *testing.T) {
	e := newRetireEnv(t, archiveCfg(), lostTorrent("a"))
	if err := e.store.Append(Record{Hash: "a", Name: "旧记录", RetiredAt: 123, Reason: ReasonLostFiles}); err != nil {
		t.Fatal(err)
	}
	e.r.scan()
	if len(e.deleteCalls()) != 0 {
		t.Error("已退休的任务不该再发删除请求")
	}
	if rec, _ := e.store.Get("a"); rec.Name != "旧记录" || rec.RetiredAt != 123 {
		t.Errorf("不该覆盖已退休记录，got %+v", rec)
	}
}

func TestRetirer_DisabledDoesNothing(t *testing.T) {
	ac := archiveCfg()
	ac.Enabled = false
	e := newRetireEnv(t, ac, lostTorrent("a"))
	e.r.scan()
	if len(e.deleteCalls()) != 0 || e.infoCount() != 0 || e.store.Len() != 0 {
		t.Error("未启用时不该有任何动作")
	}
}

func TestRetirer_UsesCacheNotFullTable(t *testing.T) {
	e := newRetireEnv(t, archiveCfg(), lostTorrent("direct"))
	e.setMaindata(`{"rid":1,"full_update":true,"torrents":{
		"cached":{"hash":"cached","name":"from-cache","state":"missingFiles","progress":1,"completion_on":2000,"added_on":1000}
	},"server_state":{}}`)
	cache := qb.NewTorrentCache(e.srv.URL, "u", "p", "")
	if err := cache.Refresh(); err != nil {
		t.Fatalf("快照同步失败: %v", err)
	}
	e.r.SetTorrentCache(cache)

	e.r.scan()
	if e.infoCount() != 0 {
		t.Errorf("快照可用时不该直连全表，实际 %d 次", e.infoCount())
	}
	if _, ok := e.store.Get("cached"); !ok {
		t.Error("应退休快照里的任务")
	}
	if _, ok := e.store.Get("direct"); ok {
		t.Error("不该读到直连全表里的任务")
	}
}

func TestRetirer_ManualRetireBypassesDryRun(t *testing.T) {
	ac := archiveCfg()
	ac.DryRun = boolPtr(true)
	e := newRetireEnv(t, ac, lostTorrent("a"), lostTorrent("b"))

	n, err := e.r.ManualRetire([]string{"a", "not-in-qb"})
	if err != nil {
		t.Fatalf("ManualRetire: %v", err)
	}
	if n != 1 {
		t.Fatalf("只应移除快照里还在的 a，got %d", n)
	}
	rec, ok := e.store.Get("a")
	if !ok || rec.Reason != ReasonManual || rec.RetiredAt == 0 {
		t.Fatalf("手动退休应记 manual 并回填时刻，got %+v ok=%v", rec, ok)
	}
	if _, ok := e.store.Get("b"); ok {
		t.Error("没点选的任务不该被建档")
	}
	calls := e.deleteCalls()
	if len(calls) != 1 || calls[0].files {
		t.Fatalf("手动退休同样不能删文件，got %+v", calls)
	}
}

func TestRetirer_ManualRetireEmpty(t *testing.T) {
	e := newRetireEnv(t, archiveCfg(), lostTorrent("a"))
	n, err := e.r.ManualRetire(nil)
	if n != 0 || err != nil {
		t.Fatalf("空列表应直接返回，got %d %v", n, err)
	}
	if e.infoCount() != 0 {
		t.Error("空列表不该拉全表")
	}
}

// 账本记录必须带齐检索需要的元数据：任务从 qB 移除后这是唯一线索
func TestRetirer_RecordCarriesMetadata(t *testing.T) {
	tor := lostTorrent("a")
	e := newRetireEnv(t, archiveCfg(), tor)
	e.r.scan()
	rec, ok := e.store.Get("a")
	if !ok {
		t.Fatal("未建档")
	}
	if rec.Name != tor.Name || rec.Category != tor.Category || rec.Tags != tor.Tags ||
		rec.SavePath != tor.SavePath || rec.ContentPath != tor.ContentPath || rec.Tracker != tor.Tracker {
		t.Errorf("字符串字段丢失：%+v", rec)
	}
	if rec.Size != tor.Size || rec.Uploaded != tor.Uploaded || rec.AddedOn != tor.AddedOn ||
		rec.CompletedOn != tor.CompletedOn || rec.Ratio != tor.Ratio {
		t.Errorf("数值字段丢失：%+v", rec)
	}
	want := "magnet:?xt=urn:btih:a&dn=" + url.QueryEscape(tor.Name) + "&tr=" + url.QueryEscape(tor.Tracker)
	if rec.Magnet != want {
		t.Errorf("磁链不符\n got %s\nwant %s", rec.Magnet, want)
	}
}

func TestBuildMagnet(t *testing.T) {
	cases := []struct {
		hash, name, tracker string
		want                string
	}{
		{"abc", "Movie 2024", "http://tr/a?x=1", "magnet:?xt=urn:btih:abc&dn=Movie+2024&tr=http%3A%2F%2Ftr%2Fa%3Fx%3D1"},
		{"abc", "", "", "magnet:?xt=urn:btih:abc"},
		{"abc", "影片", "", "magnet:?xt=urn:btih:abc&dn=%E5%BD%B1%E7%89%87"},
		{"", "n", "tr", ""},
	}
	for _, c := range cases {
		if got := BuildMagnet(c.hash, c.name, c.tracker); got != c.want {
			t.Errorf("BuildMagnet(%q,%q,%q)\n got %s\nwant %s", c.hash, c.name, c.tracker, got, c.want)
		}
	}
}
