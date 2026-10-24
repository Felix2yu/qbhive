package limiter

import (
	"testing"

	"github.com/Felix2yu/qbhive/internal/models"
)

// ============ 纯函数：比率压速公式与迟滞判定 ============

func TestRatioScaledLimit(t *testing.T) {
	cases := []struct {
		name          string
		baseKB        int
		target, ratio float64
		want          int
	}{
		{"比率未达目标保持基准", 10240, 1, 0.5, 10240},
		{"比率等于目标保持基准", 10240, 1, 1, 10240},
		{"比率零值保持基准", 10240, 1, 0, 10240},
		{"四倍比率压到四分之一", 10240, 1, 4, 2560},
		{"目标非 1 时按比例缩放", 10240, 0.5, 2, 2560},
		{"结果四舍五入", 1000, 1, 3, 333},
		{"极端比率有 1KB/s 下限", 10240, 1, 100000, 1},
		{"基准不限制时无从缩放", 0, 1, 100, 0},
		{"未启用比率维度", 10240, 0, 100, 10240},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ratioScaledLimit(tc.baseKB, tc.target, tc.ratio); got != tc.want {
				t.Errorf("ratioScaledLimit(%d, %.2f, %.2f) = %d want %d",
					tc.baseKB, tc.target, tc.ratio, got, tc.want)
			}
		})
	}
}

func TestWithinRatioDeadband(t *testing.T) {
	cases := []struct {
		name       string
		prev, next int64
		want       bool
	}{
		{"相差 5% 跳过", 1048576, 998400, true},
		{"恰好 10% 跳过", 1000000, 900000, true},
		{"相差一半需要下发", 1048576, 524288, false},
		{"回升 10% 以内跳过", 524288, 576716, true},
		{"回升超过 10% 需要下发", 524288, 1048576, false},
		{"上次为不限制(-1) 需要下发", -1, 1024, false},
		{"新值为 0 不跳过", 1048576, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := withinRatioDeadband(tc.prev, tc.next); got != tc.want {
				t.Errorf("withinRatioDeadband(%d, %d) = %v want %v", tc.prev, tc.next, got, tc.want)
			}
		})
	}
}

// ============ apply()：比率维度接入匹配与下发链路 ============

func TestLimiter_apply_RatioScaledContinuous(t *testing.T) {
	env := newLimiterEnv(t, []models.LimitRule{
		{ID: "r1", Name: "4k", Enabled: true, Match: `(?i)4k`, UploadLimit: 10240, RatioTarget: 1},
	}, `[
		{"hash":"a","name":"Movie.4k.A.mkv","ratio":0.5,"uploaded":1048576},
		{"hash":"b","name":"Movie.4k.B.mkv","ratio":1,"uploaded":2097152},
		{"hash":"c","name":"Movie.4k.C.mkv","ratio":4,"uploaded":8388608},
		{"hash":"d","name":"Movie.4k.D.mkv","ratio":100000,"uploaded":209715200}
	]`)
	env.l.apply()

	got := parseCalls(&env.limitCalls)
	want := map[string]string{"a": "10485760", "b": "10485760", "c": "2621440", "d": "1024"}
	for h, w := range want {
		if got[h] != w {
			t.Errorf("hash %s limit=%s want %s", h, got[h], w)
		}
	}
}

func TestLimiter_apply_MinUploadedGate(t *testing.T) {
	env := newLimiterEnv(t, []models.LimitRule{
		{ID: "r1", Name: "4k", Enabled: true, Match: `(?i)4k`, UploadLimit: 10240, RatioTarget: 1, MinUploaded: 500},
	}, `[
		{"hash":"tiny","name":"Tiny.4k.mkv","ratio":100,"uploaded":104857600},
		{"hash":"big","name":"Big.4k.mkv","ratio":100,"uploaded":629145600}
	]`)
	env.l.apply()

	got := parseCalls(&env.limitCalls)
	// 100MB 没到 500MB 门槛：比率再高也保持基准限速
	if got["tiny"] != "10485760" {
		t.Errorf("门槛内应压速前保持基准，got tiny=%s want 10485760", got["tiny"])
	}
	// 600MB 已过门槛：10240 × 1/100 = 102KB/s
	if got["big"] != "104448" {
		t.Errorf("过门槛后应按比率压速，got big=%s want 104448", got["big"])
	}
}

func TestLimiter_apply_RatioNeedsFiniteBase(t *testing.T) {
	env := newLimiterEnv(t, []models.LimitRule{
		{ID: "r1", Name: "unlimited", Enabled: true, Match: `(?i)free`, RatioTarget: 1},
		{ID: "r2", Name: "legacy", Enabled: true, Match: `(?i)old`, UploadLimit: 10240},
	}, `[
		{"hash":"free","name":"Free.4k.mkv","ratio":100,"uploaded":1073741824},
		{"hash":"old","name":"Old.4k.mkv","ratio":100,"uploaded":1073741824}
	]`)
	env.l.apply()

	got := parseCalls(&env.limitCalls)
	// 基准「不限制」没有可压降的起点，保持 -1 而不是压成低速
	if got["free"] != "-1" {
		t.Errorf("基准不限制时不应被比率压速，got free=%s want -1", got["free"])
	}
	// 没配比率目标的旧规则行为不变，不受比率影响
	if got["old"] != "10485760" {
		t.Errorf("旧规则应保持基准，got old=%s want 10485760", got["old"])
	}
}

func TestLimiter_apply_RatioDeadbandSuppressesCalls(t *testing.T) {
	env := newLimiterEnv(t, []models.LimitRule{
		{ID: "r1", Name: "4k", Enabled: true, Match: `(?i)4k`, UploadLimit: 10240, RatioTarget: 1},
	}, `[{"hash":"h1","name":"Movie.4k.mkv","ratio":10,"uploaded":1073741824}]`)

	env.l.apply()
	if n := len(env.calls()); n != 1 {
		t.Fatalf("首轮应下发一次，got %d 次", n)
	}
	// 1048576 B/s（1024KB/s）

	// 比率 10 → 10.5：压速值 998400，与上次相差不足 10%，不再调用 qB
	env.setTorrents(`[{"hash":"h1","name":"Movie.4k.mkv","ratio":10.5,"uploaded":1124336128}]`)
	env.l.apply()
	if n := len(env.calls()); n != 1 {
		t.Errorf("10%% 以内的比率漂移应跳过下发，got %d 次", n)
	}

	// 比率 10.5 → 20：限速减半，超出迟滞带，重新下发
	env.setTorrents(`[{"hash":"h1","name":"Movie.4k.mkv","ratio":20,"uploaded":2147483648}]`)
	env.l.apply()
	calls := env.calls()
	if len(calls) != 2 {
		t.Fatalf("超出迟滞带应重新下发，got %d 次: %v", len(calls), calls)
	}
	if calls[1] != "h1:524288" {
		t.Errorf("want h1:524288 got %s", calls[1])
	}
}

func TestLimiter_apply_FixedRuleStillExactDedup(t *testing.T) {
	// 未启用比率维度的规则不受迟滞影响：只要算出的值和上次不同就立刻下发
	env := newLimiterEnv(t, []models.LimitRule{
		{ID: "r1", Name: "4k", Enabled: true, Match: `(?i)4k`, UploadLimit: 10240},
	}, `[{"hash":"h1","name":"Movie.4k.mkv"}]`)
	env.l.apply()
	before := len(env.calls())

	cfg := env.l.cfg.Get()
	cfg.Limiter.Rules[0].UploadLimit = 9728 // 只降 5%，落在比率迟滞带内
	env.l.cfg.Set(cfg)
	env.l.Reload(nil) // Reload 清空 lastLimits，规则改动必须立刻生效
	env.l.apply()
	if after := len(env.calls()); after != before+1 {
		t.Errorf("固定限速规则改动应立刻下发，got %d -> %d", before, after)
	}
}
