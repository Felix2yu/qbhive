package archive

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

// fakeDeleter 记录守卫判定要删哪些文件，并真的删掉，
// 这样既能断言判定集合，也能断言磁盘结果
type fakeDeleter struct {
	mu      sync.Mutex
	asked   []string
	removed []string
}

func (f *fakeDeleter) AuditGuardDelete(_ models.QBTorrent, path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, path)
	if err := os.Remove(path); err != nil {
		return false
	}
	f.removed = append(f.removed, path)
	return true
}

func (f *fakeDeleter) askedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.asked...)
}

type guardEnv struct {
	g     *Guard
	store *Store
	cfg   *config.Manager
	del   *fakeDeleter
	srv   *httptest.Server
	cache *qb.TorrentCache

	mu        sync.Mutex
	deletes   []deleteCall
	filesN    int
	filesBody string
	filesCode int
	infoN     int
	maindata  string
	notices   []string
}

func (e *guardEnv) deleteCalls() []deleteCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]deleteCall(nil), e.deletes...)
}

func (e *guardEnv) filesCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.filesN
}

func (e *guardEnv) infoCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.infoN
}

func (e *guardEnv) notified() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.notices...)
}

func (e *guardEnv) setFiles(body string, code int) {
	e.mu.Lock()
	e.filesBody, e.filesCode = body, code
	e.mu.Unlock()
}

// setTorrents 换掉快照内容：full_update 会整表替换，等价于 qB 里现在是这批任务
func (e *guardEnv) setTorrents(t *testing.T, list []models.QBTorrent) {
	t.Helper()
	inner := make(map[string]json.RawMessage, len(list))
	for _, tor := range list {
		raw, err := json.Marshal(tor)
		if err != nil {
			t.Fatalf("序列化任务: %v", err)
		}
		inner[tor.Hash] = raw
	}
	body, err := json.Marshal(map[string]any{
		"rid": 1, "full_update": true, "torrents": inner, "server_state": map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.maindata = string(body)
	e.mu.Unlock()
	if err := e.cache.Refresh(); err != nil {
		t.Fatalf("快照同步失败: %v", err)
	}
}

func guardCfg(mode string) models.ArchiveConfig {
	ac := archiveCfg()
	ac.Guard = models.GuardConfig{
		Mode: mode, Interval: 5, MaxPerRound: 20, NotifyCooldownMinutes: 10,
	}
	return ac
}

func newGuardEnv(t *testing.T, ac models.ArchiveConfig) *guardEnv {
	t.Helper()
	dir := t.TempDir()
	e := &guardEnv{
		del:       &fakeDeleter{},
		filesBody: "[]",
		filesCode: http.StatusOK,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		e.infoN++
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
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
	mux.HandleFunc("/api/v2/torrents/files", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		e.filesN++
		body, code := e.filesBody, e.filesCode
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
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
		// 任务移除后 qB 就没有它的元数据了：文件列表随之变空。
		// 守卫必须「先取文件列表再移除任务」，顺序反了这里就会露出来
		e.filesBody = "[]"
		e.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)

	e.cfg = config.New(filepath.Join(dir, "config.json"))
	cfg := e.cfg.Get()
	cfg.Archive = ac
	e.cfg.Set(cfg)

	e.store = NewStore(filepath.Join(dir, "archive.jsonl"))
	e.g = NewGuard(e.cfg, qb.New(e.srv.URL, "u", "p", ""), e.store, e.del)
	e.g.SetNotifier(func(title, body string) {
		e.mu.Lock()
		e.notices = append(e.notices, title+"|"+body)
		e.mu.Unlock()
	})
	e.cache = qb.NewTorrentCache(e.srv.URL, "u", "p", "")
	e.g.SetTorrentCache(e.cache)
	return e
}

// retiredRecord 一条已退休的账本记录
func retiredRecord(hash string, retiredAt int64) Record {
	return Record{
		Hash: hash, Name: "影片." + hash, SavePath: "/data/电影",
		CompletedOn: retiredAt - 100, RetiredAt: retiredAt, Reason: ReasonLostFiles,
		Magnet: BuildMagnet(hash, "影片."+hash, ""),
	}
}

// readded 退休后又被重新添加的任务
func readded(hash string, addedOn int64, savePath string) models.QBTorrent {
	return models.QBTorrent{
		Hash: hash, Name: "影片." + hash, State: "downloading", Progress: 0.1,
		AddedOn: addedOn, SavePath: savePath,
	}
}

// writeAged 造一个指定 mtime 的文件
func writeAged(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func filesJSON(names ...string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		raw, _ := json.Marshal(models.QBFile{Name: n, Size: 4, Progress: 0.1})
		b.Write(raw)
	}
	b.WriteByte(']')
	return b.String()
}

func TestGuard_OffDoesNothing(t *testing.T) {
	e := newGuardEnv(t, guardCfg(models.GuardModeOff))
	if err := e.store.Append(retiredRecord("a", 1000)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", 2000, "/data")})
	e.g.scan()
	if len(e.deleteCalls()) != 0 || e.filesCount() != 0 || len(e.notified()) != 0 {
		t.Error("off 档不该有任何动作")
	}
}

// 守卫没有直连兜底：5s 一次直连全表比要修的问题更糟
func TestGuard_NoDirectFallback(t *testing.T) {
	// 快照未就绪
	e := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
	if err := e.store.Append(retiredRecord("a", 1000)); err != nil {
		t.Fatal(err)
	}
	e.g.scan()
	if len(e.deleteCalls()) != 0 || e.infoCount() != 0 {
		t.Error("快照未就绪应跳过本轮，绝不直连全表")
	}

	// 完全没注入快照
	e2 := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
	if err := e2.store.Append(retiredRecord("a", 1000)); err != nil {
		t.Fatal(err)
	}
	e2.g.SetTorrentCache(nil)
	e2.g.scan()
	if len(e2.deleteCalls()) != 0 || e2.infoCount() != 0 {
		t.Error("未注入快照应完全不扫")
	}
}

func TestGuard_NotifyOnlyTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	added := time.Now()
	writeAged(t, filepath.Join(dir, "new.mkv"), added.Add(10*time.Second))
	e := newGuardEnv(t, guardCfg(models.GuardModeNotify))
	if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", added.Unix(), dir)})
	e.setFiles(filesJSON("new.mkv"), http.StatusOK)

	e.g.scan()
	if len(e.deleteCalls()) != 0 {
		t.Error("notify 档不该移除任务")
	}
	if e.filesCount() != 0 {
		t.Error("notify 档连文件列表都不该取")
	}
	if !exists(filepath.Join(dir, "new.mkv")) {
		t.Error("notify 档不该动文件")
	}
	notices := e.notified()
	if len(notices) != 1 {
		t.Fatalf("应发一条告警，got %v", notices)
	}
	// 告警必须带判定依据，供干跑核对
	for _, want := range []string{"影片.a", "归档完成时间", "归档退休时间", "本次添加时间", dir} {
		if !strings.Contains(notices[0], want) {
			t.Errorf("告警缺少判定依据 %q：%s", want, notices[0])
		}
	}
}

func TestGuard_RemoveKeepsFiles(t *testing.T) {
	dir := t.TempDir()
	added := time.Now()
	writeAged(t, filepath.Join(dir, "new.mkv"), added.Add(10*time.Second))
	e := newGuardEnv(t, guardCfg(models.GuardModeRemove))
	if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", added.Unix(), dir)})

	e.g.scan()
	calls := e.deleteCalls()
	if len(calls) != 1 || calls[0].files {
		t.Fatalf("remove 档应移除任务且不删文件，got %+v", calls)
	}
	if e.filesCount() != 0 {
		t.Error("remove 档不该取文件列表")
	}
	if !exists(filepath.Join(dir, "new.mkv")) {
		t.Error("remove 档刻意留下孤儿半成品，不该动文件")
	}
}

// 核心判据：只删本次重下写出来的文件。
// 早于添加时刻的完好原片、以及不在种子元数据里的转码产物，一个都不能动。
func TestGuard_RemoveFilesSparesOldOriginalAndProduct(t *testing.T) {
	dir := t.TempDir()
	added := time.Now().Truncate(time.Second)
	orig := filepath.Join(dir, "old.mkv")
	fresh := filepath.Join(dir, "new.mkv")
	product := filepath.Join(dir, "new.mp4") // 转码产物：与原片同名不同扩展名
	writeAged(t, orig, added.Add(-time.Hour))
	writeAged(t, fresh, added.Add(10*time.Second))
	writeAged(t, product, added.Add(10*time.Second))

	e := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
	if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", added.Unix(), dir)})
	// 产物不在种子元数据里，torrents/files 根本不报它
	e.setFiles(filesJSON("old.mkv", "new.mkv"), http.StatusOK)

	e.g.scan()
	if got := e.del.askedPaths(); len(got) != 1 || got[0] != fresh {
		t.Fatalf("只该判定删除 %s，got %v", fresh, got)
	}
	if exists(fresh) {
		t.Error("本次重下写出的文件应被删掉")
	}
	if !exists(orig) {
		t.Error("mtime 早于添加时刻的完好原片绝不能删")
	}
	if !exists(product) {
		t.Error("转码产物绝不能删")
	}
	calls := e.deleteCalls()
	if len(calls) != 1 || calls[0].files {
		t.Fatalf("移除任务必须 deleteFiles=false，got %+v", calls)
	}
}

// mtime 恰好等于添加时刻算「本次写出的」（判据是 Before，不是 !After）
func TestGuard_MtimeBoundary(t *testing.T) {
	for _, delta := range []time.Duration{-time.Second, 0, time.Second} {
		dir := t.TempDir()
		added := time.Now().Truncate(time.Second)
		p := filepath.Join(dir, "f.mkv")
		writeAged(t, p, added.Add(delta))
		e := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
		if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
			t.Fatal(err)
		}
		e.setTorrents(t, []models.QBTorrent{readded("a", added.Unix(), dir)})
		e.setFiles(filesJSON("f.mkv"), http.StatusOK)
		e.g.scan()

		wantDeleted := delta >= 0
		if got := exists(p); got == wantDeleted {
			t.Errorf("mtime 偏移 %s：存在=%v，期望存在=%v", delta, got, !wantDeleted)
		}
	}
}

// qB 的未完成临时名 .!qB：报告名与去尾缀名两个候选都要能命中
func TestGuard_PartialSuffixCandidates(t *testing.T) {
	cases := []struct {
		reported string
		onDisk   string
	}{
		{"f.mkv.!qB", "f.mkv.!qB"},
		{"f.mkv.!qB", "f.mkv"},
		{"f.mkv", "f.mkv"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		added := time.Now().Truncate(time.Second)
		p := filepath.Join(dir, c.onDisk)
		writeAged(t, p, added.Add(10*time.Second))
		e := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
		if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
			t.Fatal(err)
		}
		e.setTorrents(t, []models.QBTorrent{readded("a", added.Unix(), dir)})
		e.setFiles(filesJSON(c.reported), http.StatusOK)
		e.g.scan()
		if exists(p) {
			t.Errorf("报告 %q 磁盘 %q：应删掉 %s", c.reported, c.onDisk, p)
		}
	}
}

// 去掉尾缀后的候选同样要过 mtime 闸门：不能因为换了个名字就放过旧原片
func TestGuard_PartialSuffixStillChecksMtime(t *testing.T) {
	dir := t.TempDir()
	added := time.Now().Truncate(time.Second)
	p := filepath.Join(dir, "f.mkv")
	writeAged(t, p, added.Add(-time.Hour))
	e := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
	if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", added.Unix(), dir)})
	e.setFiles(filesJSON("f.mkv.!qB"), http.StatusOK)
	e.g.scan()
	if !exists(p) {
		t.Error("去尾缀命中的旧原片同样不能删")
	}
}

func TestGuard_OutOfBoundsIgnored(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "save")
	outside := filepath.Join(base, "escape.mkv")
	added := time.Now().Truncate(time.Second)
	writeAged(t, outside, added.Add(10*time.Second))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
	if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", added.Unix(), dir)})
	e.setFiles(filesJSON("../escape.mkv"), http.StatusOK)
	e.g.scan()
	if len(e.del.askedPaths()) != 0 {
		t.Errorf("越界路径必须忽略，got %v", e.del.askedPaths())
	}
	if !exists(outside) {
		t.Error("保存目录之外的文件绝不能删")
	}
}

func TestGuard_NeverRemovesDirectory(t *testing.T) {
	dir := t.TempDir()
	added := time.Now().Truncate(time.Second)
	sub := filepath.Join(dir, "sub.mkv")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sub, added.Add(time.Minute), added.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	e := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
	if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", added.Unix(), dir)})
	e.setFiles(filesJSON("sub.mkv"), http.StatusOK)
	e.g.scan()
	if len(e.del.askedPaths()) != 0 || !exists(sub) {
		t.Error("目录绝不能删")
	}
}

// qbhive 看不到存储（Docker 没挂卷）：降级为只移除任务，不删任何文件
func TestGuard_StorageInvisibleDegrades(t *testing.T) {
	added := time.Now().Truncate(time.Second)
	e := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
	if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", added.Unix(), "/nonexistent/qbhive-test")})
	e.setFiles(filesJSON("f.mkv"), http.StatusOK)
	e.g.scan()

	if len(e.del.askedPaths()) != 0 {
		t.Error("stat 不到就不该删")
	}
	if len(e.deleteCalls()) != 1 {
		t.Error("仍应移除任务")
	}
	notices := e.notified()
	if len(notices) != 1 || !strings.Contains(notices[0], "看不到") {
		t.Fatalf("应告知已降级，got %v", notices)
	}
}

func TestGuard_FilesAPIFailureDegrades(t *testing.T) {
	dir := t.TempDir()
	added := time.Now().Truncate(time.Second)
	p := filepath.Join(dir, "f.mkv")
	writeAged(t, p, added.Add(10*time.Second))
	e := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
	if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", added.Unix(), dir)})
	// 非 JSON 响应体：GetTorrentFiles 只在解析失败时报错，不看 HTTP 状态码
	e.setFiles("upstream exploded", http.StatusInternalServerError)
	e.g.scan()

	if !exists(p) {
		t.Error("取不到文件列表时不该删任何文件")
	}
	if len(e.deleteCalls()) != 1 {
		t.Error("仍应移除任务")
	}
	if notices := e.notified(); len(notices) != 1 || !strings.Contains(notices[0], "看不到") {
		t.Fatalf("应告知已降级，got %v", notices)
	}
}

// 坑 3：正在退休 / 刚退休的任务不能被守卫当成重复下载。
// added_on 与 RetiredAt 都是秒刻度，相等时必须放过。
func TestGuard_AddedOnMustBeAfterRetiredAt(t *testing.T) {
	for _, delta := range []int64{-100, -1, 0, 1, 100} {
		e := newGuardEnv(t, guardCfg(models.GuardModeRemove))
		if err := e.store.Append(retiredRecord("a", 1000)); err != nil {
			t.Fatal(err)
		}
		e.setTorrents(t, []models.QBTorrent{readded("a", 1000+delta, "/data")})
		e.g.scan()
		want := delta > 0
		if got := len(e.deleteCalls()) == 1; got != want {
			t.Errorf("added_on 偏移 %d：命中=%v want=%v", delta, got, want)
		}
	}
}

// RetiredAt==0 是干跑候选或正在退休，任务本来就该在 qB 里
func TestGuard_PendingRecordIsNotAHit(t *testing.T) {
	e := newGuardEnv(t, guardCfg(models.GuardModeRemove))
	rec := retiredRecord("a", 1000)
	rec.RetiredAt = 0
	if err := e.store.Append(rec); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", 2000, "/data")})
	e.g.scan()
	if len(e.deleteCalls()) != 0 {
		t.Error("未退休的记录不该触发守卫")
	}
}

func TestGuard_NotInStoreIsNotAHit(t *testing.T) {
	e := newGuardEnv(t, guardCfg(models.GuardModeRemove))
	e.setTorrents(t, []models.QBTorrent{readded("a", 2000, "/data")})
	e.g.scan()
	if len(e.deleteCalls()) != 0 {
		t.Error("账本里没有的任务不该被动")
	}
}

func TestGuard_VolatileStatesSkipped(t *testing.T) {
	for _, state := range []string{"checkingDL", "checkingUP", "checkingResumeData", "moving"} {
		dir := t.TempDir()
		added := time.Now().Truncate(time.Second)
		p := filepath.Join(dir, "f.mkv")
		writeAged(t, p, added.Add(10*time.Second))
		e := newGuardEnv(t, guardCfg(models.GuardModeRemoveFiles))
		if err := e.store.Append(retiredRecord("a", added.Unix()-100)); err != nil {
			t.Fatal(err)
		}
		tor := readded("a", added.Unix(), dir)
		tor.State = state
		e.setTorrents(t, []models.QBTorrent{tor})
		e.setFiles(filesJSON("f.mkv"), http.StatusOK)
		e.g.scan()
		if len(e.deleteCalls()) != 0 || !exists(p) {
			t.Errorf("%s 态下不该动手", state)
		}
	}
}

// 单轮熔断：候选异常多通常是账本误导入，此时批量删文件不可接受
func TestGuard_MaxPerRoundBreaker(t *testing.T) {
	dir := t.TempDir()
	added := time.Now().Truncate(time.Second)
	p := filepath.Join(dir, "f.mkv")
	writeAged(t, p, added.Add(10*time.Second))

	ac := guardCfg(models.GuardModeRemoveFiles)
	ac.Guard.MaxPerRound = 20
	e := newGuardEnv(t, ac)
	var list []models.QBTorrent
	for i := 0; i < 21; i++ {
		h := "h" + strconv.Itoa(i)
		if err := e.store.Append(retiredRecord(h, added.Unix()-100)); err != nil {
			t.Fatal(err)
		}
		list = append(list, readded(h, added.Unix(), dir))
	}
	e.setTorrents(t, list)
	e.setFiles(filesJSON("f.mkv"), http.StatusOK)
	e.g.scan()

	if len(e.deleteCalls()) != 0 || len(e.del.askedPaths()) != 0 {
		t.Error("熔断时一个都不该动手")
	}
	if !exists(p) {
		t.Error("熔断时不该删文件")
	}
	notices := e.notified()
	if len(notices) != 1 || !strings.Contains(notices[0], "熔断") {
		t.Fatalf("应发一条熔断告警，got %v", notices)
	}

	// 恰好等于上限不熔断
	e2 := newGuardEnv(t, ac)
	var list2 []models.QBTorrent
	for i := 0; i < 20; i++ {
		h := "k" + strconv.Itoa(i)
		if err := e2.store.Append(retiredRecord(h, added.Unix()-100)); err != nil {
			t.Fatal(err)
		}
		list2 = append(list2, readded(h, added.Unix(), dir))
	}
	e2.setTorrents(t, list2)
	e2.g.scan()
	if len(e2.deleteCalls()) != 20 {
		t.Errorf("恰好 20 条应正常处置，got %d 次删除", len(e2.deleteCalls()))
	}
}

func TestGuard_NotifyCooldown(t *testing.T) {
	ac := guardCfg(models.GuardModeNotify)
	ac.Guard.NotifyCooldownMinutes = 10
	e := newGuardEnv(t, ac)
	if err := e.store.Append(retiredRecord("a", 1000)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", 2000, "/data")})

	e.g.scan()
	e.g.scan()
	e.g.scan()
	if got := len(e.notified()); got != 1 {
		t.Fatalf("冷却期内应只告警一次，got %d", got)
	}

	// 冷却期满后应再告警
	e.g.mu.Lock()
	e.g.lastNotify["a"] = time.Now().Unix() - 11*60
	e.g.mu.Unlock()
	e.g.scan()
	if got := len(e.notified()); got != 2 {
		t.Fatalf("冷却期满应再告警，got %d", got)
	}
}

// 冷却只压制通知，不压制处置动作：否则冷却期内的重下就漏过去了
func TestGuard_CooldownDoesNotSuppressAction(t *testing.T) {
	e := newGuardEnv(t, guardCfg(models.GuardModeRemove))
	if err := e.store.Append(retiredRecord("a", 1000)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", 2000, "/data")})
	e.g.scan()
	e.g.scan()
	if got := len(e.deleteCalls()); got != 2 {
		t.Errorf("两轮都该提交移除，got %d", got)
	}
	if got := len(e.notified()); got != 1 {
		t.Errorf("冷却期内只该告警一次，got %d", got)
	}
}

func TestGuard_BurstCooldown(t *testing.T) {
	ac := guardCfg(models.GuardModeRemoveFiles)
	ac.Guard.MaxPerRound = 1
	e := newGuardEnv(t, ac)
	var list []models.QBTorrent
	for _, h := range []string{"a", "b"} {
		if err := e.store.Append(retiredRecord(h, 1000)); err != nil {
			t.Fatal(err)
		}
		list = append(list, readded(h, 2000, "/data"))
	}
	e.setTorrents(t, list)
	e.g.scan()
	e.g.scan()
	if got := len(e.notified()); got != 1 {
		t.Errorf("熔断告警也该冷却，got %d", got)
	}
}

func TestGuard_UnknownModeIsInert(t *testing.T) {
	e := newGuardEnv(t, guardCfg("bogus"))
	if err := e.store.Append(retiredRecord("a", 1000)); err != nil {
		t.Fatal(err)
	}
	e.setTorrents(t, []models.QBTorrent{readded("a", 2000, "/data")})
	e.g.scan()
	if len(e.deleteCalls()) != 0 || len(e.notified()) != 0 {
		t.Error("未知档位不该有任何动作")
	}
}
