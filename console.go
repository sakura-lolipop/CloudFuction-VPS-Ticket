package main

// /console 观察面板（2026-08-25 加，stats 单独 CP）：内存计数 + 最近日志 ring + 单页 HTML。
// 样式 = NEXT-Server /console 同款 Tabler dist（web/tabler.min.css go:embed，二进制自包含
// 不引 CDN——联邦件零外依赖）；显示层标记用 ASCII（OK/ERR/BAN/LIMIT，避免字形缺字体豆腐块）。
// 免鉴权直接进（观察数据不敏感，自己节点的量/IP/状态码）；JSON 模式 ?json=1。
// 数据单锁粗粒度（签票低频，不存在的竞争不设计）。

import (
	_ "embed"
	"fmt"
	"html"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/tabler.min.css
var tablerCSS []byte

const statsRingSize = 200 // 最近日志行数（面板黑窗容量）

type statsState struct {
	mu          sync.Mutex
	started     time.Time
	status      map[int]int64   // 状态码 → 次数
	ips         map[string]int64 // 签发成功 per-IP（Top 表）
	issued      int64
	issuedToday int64
	today       string // "2006/01/02"（日期滚动清零 today 桶）
	bans        int64  // auto-ban 触发次数
	ring        []string
}

var stats = newStats()

func newStats() *statsState {
	return &statsState{
		started: nowFn(),
		status:  map[int]int64{},
		ips:     map[string]int64{},
		today:   nowFn().Format("2006/01/02"),
	}
}

// statsRecord 请求终态记账（reqLog 单一真相收口：漏一个分支=面板少一类数）。
func statsRecord(status int, ip string, ok bool) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	today := nowFn().Format("2006/01/02")
	if stats.today != today {
		stats.today, stats.issuedToday = today, 0 // 日期滚动：today 桶清零（总量不清）
	}
	stats.status[status]++
	if ok {
		stats.issued++
		stats.issuedToday++
		stats.ips[ip]++
	}
}

func statsRecordBan() {
	stats.mu.Lock()
	stats.bans++
	stats.mu.Unlock()
}

// ringWriter 把 log 原文（渲染前）塞进 ring（面板黑窗回放）。
type ringWriter struct{}

func (ringWriter) Write(p []byte) (int, error) {
	stats.mu.Lock()
	for _, ln := range strings.Split(strings.TrimSuffix(string(p), "\n"), "\n") {
		if ln == "" {
			continue
		}
		stats.ring = append(stats.ring, ln)
		if len(stats.ring) > statsRingSize {
			stats.ring = stats.ring[len(stats.ring)-statsRingSize:]
		}
	}
	stats.mu.Unlock()
	return len(p), nil
}

// statsSnapshot 面板/JSON 共用快照（锁内拷出）。
type statsSnapshot struct {
	Uptime      string         `json:"uptime"`
	Issued      int64          `json:"issued"`
	IssuedToday int64          `json:"issued_today"`
	Status      map[int]int64  `json:"status"`
	Bans        int64          `json:"bans"`
	TopIPs      []statsIPCount `json:"top_ips"`
	Recent      []string       `json:"recent"`
	TTL         int            `json:"ttl_seconds"`
	Mode        string         `json:"mode"`
}

type statsIPCount struct {
	IP    string `json:"ip"`
	Count int64  `json:"count"`
}

func takeStatsSnapshot() statsSnapshot {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	mode := "anonymous"
	if os.Getenv("TICKET_AUTH_TOKEN") != "" {
		mode = "token-auth"
	}
	top := make([]statsIPCount, 0, len(stats.ips))
	for ip, n := range stats.ips {
		top = append(top, statsIPCount{ip, n})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Count > top[j].Count })
	if len(top) > 10 {
		top = top[:10]
	}
	recent := append([]string(nil), stats.ring...)
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i] // 最新在顶
	}
	return statsSnapshot{
		Uptime:      nowFn().Sub(stats.started).Truncate(time.Second).String(),
		Issued:      stats.issued,
		IssuedToday: stats.issuedToday,
		Status:      stats.status,
		Bans:        stats.bans,
		TopIPs:      top,
		Recent:      recent,
		TTL:         ticketTTLSeconds(),
		Mode:        mode,
	}
}

// asciiGlyphs 显示层 Unicode 字形 → ASCII（面板全 ASCII 观感，防缺字体豆腐块）。
var asciiGlyphs = []struct{ from, to string }{
	{"✓", "OK"},
	{"✗", "ERR"},
	{"⚠", "WARN"},
	{"🧊", "BAN"},
}

func asciiize(s string) string {
	for _, g := range asciiGlyphs {
		s = strings.ReplaceAll(s, g.from, g.to)
	}
	return s
}

func handleConsoleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(tablerCSS)
}

// consoleCopy i18n 文案（zh 默认 / en；?lang= 切换，照 NEXT-Server /console 的 lang 参数先例）。
var consoleCopy = map[string]map[string]string{
	"zh": {
		"title": "Hotify 铸票台", "brand": "Hotify CF-Ticket",
		"uptime": "运行", "ttl": "票期", "anon": "匿名", "tokenauth": "token 鉴权",
		"total": "总签发", "today": "今日签发",
		"ok": "成功 200", "auth": "鉴权 401", "ban": "封禁 403", "limit": "限流 429", "err": "错误 500",
		"autoban": "自动封禁",
		"topip": "拿票最多的 IP", "recent": "最近日志", "newest": "最新在上 · 5 秒自动刷新",
		"empty": "还没有签发", "switch": "English",
	},
	"en": {
		"title": "Hotify CF-Ticket", "brand": "Hotify CF-Ticket",
		"uptime": "uptime", "ttl": "ttl", "anon": "anonymous", "tokenauth": "token-auth",
		"total": "total issued", "today": "today",
		"ok": "OK 200", "auth": "ERR 401", "ban": "BAN 403", "limit": "LIMIT 429", "err": "ERR 500",
		"autoban": "auto-ban",
		"topip": "Top issue IPs", "recent": "Recent log", "newest": "newest first · 5s refresh",
		"empty": "no issues yet", "switch": "中文",
	},
}

func consoleLang(r *http.Request) string {
	if r.URL.Query().Get("lang") == "en" {
		return "en"
	}
	return "zh" // 默认中文（用户主用）
}

// handleConsole /console（免鉴权直接进）；?json=1 出 JSON；?lang=en 英文。
func handleConsole(w http.ResponseWriter, r *http.Request) {
	snap := takeStatsSnapshot()
	if r.URL.Query().Get("json") == "1" {
		writeJSON(w, 200, snap)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, consoleHTML(snap, consoleLang(r)))
}

// consoleHTML Tabler 单页（骨架照 NEXT-Server /console 同款：page/navbar/card/datagrid）。
func consoleHTML(s statsSnapshot, lang string) string {
	c := consoleCopy[lang]
	mode := c["anon"]
	if s.Mode == "token-auth" {
		mode = c["tokenauth"]
	}
	stat := func(label string, val string, cls string) string {
		return `<div class="datagrid"><div class="datagrid-item"><div class="datagrid-content"><div class="text-secondary fs-2">` + label + `</div><div class="fs-1 ` + cls + `">` + val + `</div></div></div></div>`
	}
	statusCell := func(code int, label string, cls string) string {
		return stat(label, strconv.FormatInt(s.Status[code], 10), cls)
	}
	ipRows := strings.Builder{}
	for _, t := range s.TopIPs {
		ipRows.WriteString(`<tr><td>` + html.EscapeString(t.IP) + `</td><td class="text-end">` + strconv.FormatInt(t.Count, 10) + `</td></tr>`)
	}
	if ipRows.Len() == 0 {
		ipRows.WriteString(`<tr><td colspan="2" class="text-secondary">` + c["empty"] + `</td></tr>`)
	}
	logLines := strings.Builder{}
	for _, ln := range s.Recent {
		logLines.WriteString(html.EscapeString(asciiize(ln)) + "\n")
	}
	switchLang := "en"
	if lang == "en" {
		switchLang = "zh"
	}
	return `<!doctype html>
<html lang="` + lang + `"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="5;url=?lang=` + lang + `">
<title>` + c["title"] + `</title>
<link rel="stylesheet" href="/tabler.min.css">
</head><body>
<div class="page">
<nav class="navbar navbar-expand-md navbar-light">
  <div class="container-xl">
    <h1 class="navbar-brand mb-0 h3">` + c["brand"] + `</h1>
    <div class="text-secondary">` + c["uptime"] + ` ` + s.Uptime + ` · ` + c["ttl"] + `=` + strconv.Itoa(s.TTL) + `s · ` + mode + ` · <a href="?lang=` + switchLang + `">` + c["switch"] + `</a></div>
  </div>
</nav>
<div class="page-wrapper"><div class="page-body"><div class="container-xl">
<div class="card card-md mb-3">
  <div class="card-body">` +
	stat(c["total"], strconv.FormatInt(s.Issued, 10), "") +
	stat(c["today"], strconv.FormatInt(s.IssuedToday, 10), "") +
	statusCell(200, c["ok"], "text-blue") +
	statusCell(401, c["auth"], "text-orange") +
	statusCell(403, c["ban"], "text-orange") +
	statusCell(429, c["limit"], "text-orange") +
	statusCell(500, c["err"], "text-red") +
	stat(c["autoban"], strconv.FormatInt(s.Bans, 10), "text-secondary") +
	`</div></div>
<div class="card card-md mb-3">
  <div class="card-header"><h3 class="card-title">` + c["topip"] + `</h3></div>
  <div class="table-responsive"><table class="table table-vcenter card-table">` + ipRows.String() + `</table></div>
</div>
<div class="card card-md">
  <div class="card-header"><h3 class="card-title">` + c["recent"] + `</h3><div class="card-actions text-secondary">` + c["newest"] + `</div></div>
  <div class="card-body"><pre class="mb-0" style="background:#1e1e1e;color:#d4d4d4;border-radius:6px;padding:12px;font:12px/1.6 Consolas,Monaco,monospace;overflow-y:auto;max-height:360px;white-space:pre-wrap;word-break:break-all">` + logLines.String() + `</pre></div>
</div>
</div></div></div>
</div></body></html>`
}
