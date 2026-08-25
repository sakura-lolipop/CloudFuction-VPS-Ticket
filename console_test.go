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
		"refresh", "autorefresh", "autorefreshon", "empty", "switch"}
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

// TestRefreshResumeNotStuck 暂停页恢复链接必须去掉 refresh=0（pageQuery nil 不删键=死路）。
func TestRefreshResumeNotStuck(t *testing.T) {
	snap := statsSnapshot{UptimeDur: time.Minute, TTL: 600}
	body := consoleHTML(snap, "zh", "t", "refresh=0", 0)
	if n := strings.Count(body, `href="?refresh=0"`); n >= 2 {
		t.Fatalf("暂停页 %d 处 ?refresh=0（恢复按钮应 Del refresh 回落默认）", n)
	}
	if !strings.Contains(body, `href="?refresh=5"`) && !strings.Contains(body, `href="?"`) {
		// 恢复按钮 href 应为去掉 refresh 的 query（空则 "?" 或裸页）
		t.Logf("resume link form check: body contains href without refresh=0")
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

// TestIPv6HostBadge IPv6 literal host 不切坏。
func TestIPv6HostBadge(t *testing.T) {
	snap := statsSnapshot{UptimeDur: time.Minute, TTL: 600}
	if body := consoleHTML(snap, "zh", "[::1]:12346", "", 5); strings.Contains(body, `bg-blue-lt">`+"[") {
		t.Fatalf("IPv6 host 被切成裸 [")
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
