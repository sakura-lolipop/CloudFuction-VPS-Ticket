package main

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestConsoleNoPanic 回归：/console 三态（默认/暂停/en）不 panic 且 200——锁 2026-08-25
// new(url.Values) nil-map 事故（Add 即炸、线上表现为空回复连接断）。
func TestConsoleNoPanic(t *testing.T) {
	resetGuard()
	genTestSA(t)
	for _, q := range []string{"", "?refresh=0", "?lang=en"} {
		req := httptest.NewRequest("GET", "/console"+q, nil)
		req.Host = "ticket.hotify.love"
		rec := httptest.NewRecorder()
		handleConsole(rec, req) // panic 直接炸测试=复现
		if rec.Code != 200 {
			t.Fatalf("q=%s code=%d body=%s", q, rec.Code, rec.Body.String()[:200])
		}
	}
}

// ── 对抗审收编的永久回归（2026-08-25 三 agent findings 锁死）──

// TestConsoleCopyKeysLocked consoleHTML 引用的每个 copy key 必须两语言都有定义——
// 锁 sed 误删整行（topip/recent 曾连坐）导致的空标题带绿出厂。
func TestConsoleCopyKeysLocked(t *testing.T) {
	used := []string{"title", "brand", "uptime", "ttl", "anon", "tokenauth", "total", "today",
		"ok", "auth", "ban", "limit", "err", "autoban", "topip", "recent", "iphead", "counthead",
		"refresh", "autorefresh", "autorefreshon", "empty", "switch",
		// 2026-08-30 lookinggood：胶囊+光感面板+壁纸新键（CJ 注入 JS 侧消费）；
		// 08-30 二轮 server 对齐：ds 滑杆入注册表
		"pause", "theme", "bgshow", "bghide", "lptitle", "gain", "edge", "radius", "ds",
		"color", "blob", "reset", "themedark", "themelight"}
	for _, lang := range []string{"zh", "en"} {
		for _, key := range used {
			if consoleCopy[lang][key] == "" {
				t.Fatalf("consoleCopy[%s][%q] 空——consoleHTML 引用但文案表缺失", lang, key)
			}
		}
	}
}

// TestConsoleTitlesRender 标题真渲染进 HTML（不只 key 存在）。
func TestConsoleTitlesRender(t *testing.T) {
	snap := statsSnapshot{UptimeDur: time.Minute, TTL: 600}
	for _, c := range []struct{ lang, want string }{{"zh", "签发来源"}, {"zh", "实时日志"}, {"en", "Top sources"}, {"en", "Live log"}} {
		if body := consoleHTML(snap, c.lang, "t", "", 5); !strings.Contains(body, c.want) {
			t.Fatalf("lang=%s 缺标题 %q", c.lang, c.want)
		}
	}
}

// TestRefreshResumeNotStuck 2026-08-30 增量刷新版：禁 meta 整页刷新回归；
// 暂停态改走 localStorage（cf2-console-paused）+ ?refresh=0 显式 URL 压过记忆，两通道都要在 JS 里。
func TestRefreshResumeNotStuck(t *testing.T) {
	snap := statsSnapshot{UptimeDur: time.Minute, TTL: 600}
	for _, q := range []string{"refresh=0", "refresh=5", ""} {
		body := consoleHTML(snap, "zh", "t", q, 0)
		if strings.Contains(body, `http-equiv="refresh"`) {
			t.Fatalf("q=%s 出现 meta refresh——增量刷新版禁整页刷新回归", q)
		}
		if !strings.Contains(body, `cf2-console-paused`) {
			t.Fatalf("q=%s 缺 localStorage 暂停记忆通道", q)
		}
		if !strings.Contains(body, `fetch('?json=1')`) {
			t.Fatalf("q=%s 缺 ?json=1 轮询", q)
		}
	}
	// REFRESH_URL 线程：显式 ?refresh=0 → JS 判暂停走 URL 不走 localStorage
	if !strings.Contains(consoleHTML(snap, "zh", "t", "refresh=0", 0), `REFRESH_URL=true`) {
		t.Fatalf("显式 ?refresh=0 未传 REFRESH_URL=true")
	}
	if !strings.Contains(consoleHTML(snap, "zh", "t", "", 5), `REFRESH_URL=false`) {
		t.Fatalf("无 refresh 参数应传 REFRESH_URL=false（JS 读 localStorage）")
	}
}

// TestHumanDurZeroOmit 末位 0 省略语义（注释曾撒谎）。
func TestHumanDurZeroOmit(t *testing.T) {
	cases := []struct {
		d        time.Duration
		zh, en   string
	}{
		{10 * time.Minute, "10 分钟", "10m"},
		{24 * time.Hour, "1 天", "1d"},
		{45 * time.Second, "45 秒", "45s"},
		{0, "0 秒", "0s"},
		{time.Hour + 3*time.Minute, "1 小时 3 分钟", "1h 3m"},
	}
	for _, c := range cases {
		if got := humanDur(c.d, "zh"); got != c.zh {
			t.Fatalf("humanDur(%v,zh)=%q want %q", c.d, got, c.zh)
		}
		if got := humanDur(c.d, "en"); got != c.en {
			t.Fatalf("humanDur(%v,en)=%q want %q", c.d, got, c.en)
		}
	}
}

// TestIPv6HostBadge IPv6 literal host 不切坏（SplitHostPort 截端口剥括号，pill 显示净 ::1）。
func TestIPv6HostBadge(t *testing.T) {
	snap := statsSnapshot{UptimeDur: time.Minute, TTL: 600}
	body := consoleHTML(snap, "zh", "[::1]:12346", "", 5)
	if !strings.Contains(body, `<span class="hostpill">::1</span>`) {
		t.Fatalf("IPv6 host pill 应为净 ::1（端口截掉、不出裸 [）")
	}
}

// TestLineLightRegistryLocked 分隔线受光家族注册单一真相（2026-09-01 用户报 .hr/card-header 无光
// =线家族只有 #ipbody tr 一个成员,其余线没人管）：线通道名单是渲染器内唯一注册表,锁成员防静默删
// （grep gate 惯例——prose 不设防）。
func TestLineLightRegistryLocked(t *testing.T) {
	snap := statsSnapshot{UptimeDur: time.Minute, TTL: 600}
	body := consoleHTML(snap, "zh", "t", "", 5)
	for _, anchor := range []string{
		`SEL_ROW='#ipbody tr'`,
		`SEL_LINE_TOP='.hr'`,
		`SEL_LINE_BOT='.card-header, .card-table thead th'`,
	} {
		if !strings.Contains(body, anchor) {
			t.Fatalf("分隔线注册表缺 %q——线家族名单变更要过目", anchor)
		}
	}
}

// TestThemeTransitionLocked 主题切换 VT 圆形揭示构件（server 2026-09-01 终态同构）——
// keyframes/VT API/坐标变量/视觉视口修正四件套锚点，防静默退化回硬切。
func TestThemeTransitionLocked(t *testing.T) {
	snap := statsSnapshot{UptimeDur: time.Minute, TTL: 600}
	body := consoleHTML(snap, "zh", "t", "", 5)
	for _, anchor := range []string{
		`@keyframes vt-circle`,
		`startViewTransition(apply)`,
		`--vt-r`,
		`visualViewport`,
	} {
		if !strings.Contains(body, anchor) {
			t.Fatalf("主题切换动画构件缺 %q——VT 圆形揭示被改掉要过目", anchor)
		}
	}
}

// TestTicketTTLFloor ttl 下限 1（0=出生即过期的死票）。
func TestTicketTTLFloor(t *testing.T) {
	if v := ticketTTLSeconds(); v < 1 {
		t.Fatalf("ttl=%d <1", v)
	}
	resetGuard()
	os.Setenv("TICKET_TTL_SECONDS", "0")
	t.Cleanup(func() { os.Unsetenv("TICKET_TTL_SECONDS") })
	if v := ticketTTLSeconds(); v != 600 {
		t.Fatalf("ttl:0 → %d want 600（下限回落）", v)
	}
}

// TestCfgEnvNameTTLSeconds env 名与已发布文档一致（曾漂移 TICKET_TTL vs TICKET_TTL_SECONDS）。
func TestCfgEnvNameTTLSeconds(t *testing.T) {
	if got := cfgEnvName("ttl"); got != "TICKET_TTL_SECONDS" {
		t.Fatalf("cfgEnvName(ttl)=%q want TICKET_TTL_SECONDS", got)
	}
}

// TestParseSimpleYAMLQuotes BOM/引号剥/病行上报。
func TestParseSimpleYAMLQuotes(t *testing.T) {
	vals, bad := parseSimpleYAML("auth_token: \"sec\"\nttl: 600\ngarbage line no colon")
	if vals["auth_token"] != "sec" {
		t.Fatalf("成对引号未剥: %q", vals["auth_token"])
	}
	if vals["ttl"] != "600" {
		t.Fatalf("ttl: %q", vals["ttl"])
	}
	if len(bad) != 1 || !strings.Contains(bad[0], "garbage") {
		t.Fatalf("病行未上报: %v", bad)
	}
	vals, _ = parseSimpleYAML("\uFEFFttl: 700")
	if vals["ttl"] != "700" {
		t.Fatalf("BOM 吞首键: %q", vals["ttl"])
	}
}
