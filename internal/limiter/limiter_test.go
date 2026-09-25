package limiter

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Felix2yu/qbhive/internal/config"
	"github.com/Felix2yu/qbhive/internal/models"
	qb "github.com/Felix2yu/qbhive/internal/qbittorrent"
)

func setupLimiterWithMockQB(t *testing.T, torrentsJSON string) (*Limiter, *[]string, *httptest.Server) {
	t.Helper()
	calls := new([]string)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test")
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/torrents/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-bittorrent")
		w.Write([]byte(torrentsJSON))
	})
	mux.HandleFunc("/api/v2/torrents/setUploadLimit", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		hash := r.FormValue("hashes")
		limit := r.FormValue("limit")
		*calls = append(*calls, hash+":"+limit)
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	cfg := config.New("")
	cfg.Set(models.AppConfig{
		Limiter: models.LimiterConfig{
			Enabled: true, Interval: 10,
			Rules: []models.LimitRule{
				{ID: "r1", Name: "4k", Enabled: true, Match: `(?i)4k`, UploadLimit: 10240},
				{ID: "r2", Name: "remux", Enabled: true, Match: `(?i)remux`, UploadLimit: 5120},
				{ID: "r3", Name: "disabled", Enabled: false, Match: `test`, UploadLimit: 999},
			},
		},
	})
	cli := qb.New(srv.URL, "u", "p", "")
	return New(cfg, cli), calls, srv
}

func parseCalls(calls *[]string) map[string]string {
	m := make(map[string]string)
	for _, c := range *calls {
		h, lim, _ := strings.Cut(c, ":")
		m[h] = lim
	}
	return m
}

func TestLimiter_apply_MatchesAndLimits(t *testing.T) {
	torrents := `[{"hash":"h1","name":"Movie.4k.HDR.mkv"},{"hash":"h2","name":"Movie.1080p.mkv"},{"hash":"h3","name":"Movie.REMUX.mkv"},{"hash":"h4","name":"SomeOther"}]`
	l, calls, srv := setupLimiterWithMockQB(t, torrents)
	defer srv.Close()
	l.apply()

	got := parseCalls(calls)
	if got["h1"] != "10485760" { t.Errorf("h1 limit=%s want 10485760", got["h1"]) }
	if got["h3"] != "5242880" { t.Errorf("h3 limit=%s want 5242880", got["h3"]) }
	if got["h2"] != "" || got["h4"] != "" { t.Errorf("h2/h4 no limit: %v", got) }
}

func TestLimiter_apply_NoDuplicateOnSecondRun(t *testing.T) {
	l, calls, srv := setupLimiterWithMockQB(t, `[{"hash":"h1","name":"Movie.4k.mkv"}]`)
	defer srv.Close()
	l.apply()
	before := len(*calls)
	l.apply()
	if len(*calls) != before { t.Errorf("2nd run no-op: %d -> %d", before, len(*calls)) }
}

func TestLimiter_Reload_ClearsLastLimits(t *testing.T) {
	l, calls, srv := setupLimiterWithMockQB(t, `[{"hash":"h1","name":"Movie.4k.mkv"}]`)
	defer srv.Close()
	l.apply()
	before := len(*calls)
	l.Reload(nil)
	l.apply()
	if len(*calls) != before+1 { t.Errorf("after reload re-apply: %d -> %d", before, len(*calls)) }
}

func TestLimiter_ReloadDisabled_SetsRunningFalse(t *testing.T) {
	l, _, srv := setupLimiterWithMockQB(t, `[]`)
	defer srv.Close()
	cfg := l.cfg.Get(); cfg.Limiter.Enabled = false; l.cfg.Set(cfg)
	l.Reload(nil)
	if l.running { t.Error("disabled should not run") }
}

func TestLimiter_apply_GetTorrentsError(t *testing.T) {
	l, calls, srv := setupLimiterWithMockQB(t, `[{"hash":"h1"}]`)
	srv.Close()
	l.apply()
	if len(*calls) != 0 { t.Errorf("on error no limits: got %v", *calls) }
}

func TestLimiter_SetClient_HotSwap(t *testing.T) {
	l, _, srv := setupLimiterWithMockQB(t, `[{"hash":"h1","name":"Movie.4k.mkv"}]`)
	defer srv.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "SID=test"); w.Write([]byte("Ok."))
	}))
	defer srv2.Close()
	l.SetClient(qb.New(srv2.URL, "u", "p", ""))
	l.apply() // 不应 panic
}
