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
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/tabler.min.css
var tablerCSS []byte

//go:embed web/hotify-icon.png
var hotifyIcon []byte

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
	UptimeDur   time.Duration  `json:"-"` // 面板人性化用（humanDur）
	Issued      int64          `json:"issued"`
	IssuedToday int64          `json:"issued_today"`
	Status      map[int]int64  `json:"status"`
	Bans        int64          `json:"bans"`
	TopIPs      []statsIPCount `json:"top_ips"`
	Recent      []string       `json:"recent"`
	TTL         int            `json:"ttl_seconds"`
	Mode        string         `json:"mode"`
	ProjectID   string         `json:"project_id"`
}

type statsIPCount struct {
	IP    string `json:"ip"`
	Count int64  `json:"count"`
}

func takeStatsSnapshot() statsSnapshot {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	mode := "anonymous"
	if ticketAuthToken() != "" {
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
	proj := "-"
	if acct, err := loadSA(); err == nil {
		proj = acct.ProjectID
	}
	return statsSnapshot{
		Uptime:      nowFn().Sub(stats.started).Truncate(time.Second).String(),
		UptimeDur:   nowFn().Sub(stats.started).Truncate(time.Second),
		Issued:      stats.issued,
		IssuedToday: stats.issuedToday,
		Status:      stats.status,
		Bans:        stats.bans,
		TopIPs:      top,
		Recent:      recent,
		TTL:         ticketTTLSeconds(),
		Mode:        mode,
		ProjectID:   proj,
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

func handleConsoleIcon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(hotifyIcon)
}

// cardIcon 卡片标题图标（Tabler icons 风格内联 stroke SVG，零外部依赖；治"卡头一行小字浮着空"）。
func cardIcon(name string) string {
	var path string
	switch name {
	case "list":
		path = `<path d="M9 6h11"/><path d="M9 12h11"/><path d="M9 18h11"/><path d="M5 6v.01"/><path d="M5 12v.01"/><path d="M5 18v.01"/>`
	case "terminal":
		path = `<path d="M4 17l6-6-6-6"/><path d="M12 19h8"/>`
	default:
		return ""
	}
	return `<svg xmlns="http://www.w3.org/2000/svg" class="icon icon-tabler me-2" width="24" height="24" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round" style="vertical-align:-.375rem">` + path + `</svg>`
}

// consoleCopy i18n 文案（zh 默认 / en；?lang= 切换。2026-08-25 对抗审终稿：
// label 不带状态码——badge 是唯一码源；en 弃日志 ASCII 字形（ERR/BAN）用互译人话）。
var consoleCopy = map[string]map[string]string{
	"zh": {
		"title": "Hotify Ticket", "brand": "Hotify Ticket",
		"uptime": "已运行", "ttl": "票据有效期", "anon": "匿名开放", "tokenauth": "Token 鉴权", "project": "项目",
		"total": "累计签发", "today": "今日签发",
		"ok": "成功", "auth": "鉴权失败", "ban": "封禁拦截", "limit": "触发限流", "err": "服务错误",
		"autoban": "自动封禁",
		"topip": "签发来源", "recent": "实时日志",
		"iphead": "IP", "counthead": "签发次数",
		"refresh": "刷新", "autorefresh": "自动刷新", "autorefreshon": "自动刷新中",
		"empty": "暂无签发", "switch": "English",
	},
	"en": {
		"title": "Hotify Ticket", "brand": "Hotify Ticket",
		"uptime": "uptime", "ttl": "ttl", "anon": "anonymous", "tokenauth": "Token auth", "project": "project",
		"total": "total issued", "today": "today",
		"ok": "Success", "auth": "Unauthorized", "ban": "Banned", "limit": "Rate limited", "err": "Server error",
		"autoban": "auto-ban",
		"topip": "Top sources", "recent": "Live log",
		"iphead": "IP", "counthead": "tickets",
		"refresh": "Refresh", "autorefresh": "Auto refresh", "autorefreshon": "auto-refreshing",
		"empty": "no tickets yet", "switch": "中文",
	},
}

func consoleLang(r *http.Request) string {
	if r.URL.Query().Get("lang") == "en" {
		return "en"
	}
	return "zh" // 默认中文（用户主用）
}

// humanDur 顶栏人性化时长（取最高两档，末位 0 省略）：26h→"1 天 2 小时"，1h3m→"1 小时 3 分"，
// 10m→"10 分钟"（不带"0 秒"），45s→"45 秒"，0→"0 秒"。Duration 原样太机器味。
func humanDur(d time.Duration, lang string) string {
	zh := lang == "zh"
	u := func(n int, z, e string) string {
		if n == 0 {
			return "" // 末位 0 省略（pick 滤空；对抗审 F6：注释宣称了没实现）
		}
		if zh {
			return strconv.Itoa(n) + " " + z
		}
		return strconv.Itoa(n) + e
	}
	totalSec := int(d.Seconds())
	days, hours := totalSec/86400, totalSec%86400/3600
	mins, secs := totalSec%3600/60, totalSec%60
	pick := func(parts ...string) string {
		var out []string
		for _, p := range parts {
			if p != "" {
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			if zh {
				return "0 秒"
			}
			return "0s"
		}
		return strings.Join(out, " ")
	}
	return pick(u(days, "天", "d"), u(hours, "小时", "h"), u(mins, "分钟", "m"), u(secs, "秒", "s"))
}

// humanTTL 票期人性化：600s→"10 分钟"/"10 min"；90s→"90 秒"/"90 s"（zh 不落拉丁单位，对抗审修）。
func humanTTL(secs int, lang string) string {
	if secs%60 == 0 {
		return humanDur(time.Duration(secs)*time.Second, lang)
	}
	if lang == "zh" {
		return strconv.Itoa(secs) + " 秒"
	}
	return strconv.Itoa(secs) + "s"
}
func handleConsole(w http.ResponseWriter, r *http.Request) {
	snap := takeStatsSnapshot()
	if r.URL.Query().Get("json") == "1" {
		writeJSON(w, 200, snap)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 自动刷新默认开（2026-08-25 用户裁定：打开就是活的）；?refresh=0 暂停（query 显式 0 才停——
	// 去掉参数会回落默认 5，暂停态必须靠参数粘滞）。
	refresh := 5
	if v, err := strconv.Atoi(r.URL.Query().Get("refresh")); err == nil && v >= 0 {
		refresh = v
	}
	fmt.Fprint(w, consoleHTML(snap, consoleLang(r), r.Host, r.URL.RawQuery, refresh))
}

// pageQuery 页面链接的单一构造器（2026-08-25 收拢：此前 lang 手拼+toggleRefreshQuery 散置 3 个
// 半写者，en 页 query 已带 lang=en 再拼 &lang=zh 成双值，Get 永读第一个→切不回中文）。
// 幂等：set 的键先 Del 再 Set（单值保证），其余参数原样保留。
// ⚠️ q 必须来自 parseRawQuery（ParseQuery 返非 nil map）——new(url.Values) 是 nil map 指针，
// Add 即 panic（2026-08-25 /console 空回复事故根因，console_test 锁回归）。
func pageQuery(rawQuery string, set map[string]string) string {
	q := parseRawQuery(rawQuery)
	for k, v := range set {
		q.Del(k)
		if v != "" {
			q.Set(k, v)
		}
	}
	link := q.Encode()
	if link == "" {
		return ""
	}
	return "?" + link
}

func parseRawQuery(raw string) url.Values {
	vals, _ := url.ParseQuery(raw)
	return vals
}

// consoleHTML Tabler 单页（骨架照 NEXT-Server /console；对抗审终稿：状态卡 label 不带码——
// badge 是唯一码源且上语义色；刷新=按钮默认+?refresh=5 opt-in；project badge 走 maskProject
// 对齐日志纪律；日志卡 card-sm 收白边；Top IP 表加 thead）。
func consoleHTML(s statsSnapshot, lang string, host string, rawQuery string, refresh int) string {
	c := consoleCopy[lang]
	mode := c["anon"]
	if s.Mode == "token-auth" {
		mode = c["tokenauth"]
	}
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h // 截端口（localhost:12346→localhost；SplitHostPort 保 IPv6 [::1] 不切坏——对抗审 F5）
	}
	// statCard 平铺卡片：subheader 人话题（不带码）+ 大数字 + 语义色码角标（状态卡）。
	statCard := func(label, val, h1cls, badge, badgeCls string) string {
		tag := ""
		if badge != "" {
			tag = `<span class="badge ` + badgeCls + ` ms-2">` + badge + `</span>`
		}
		return `<div class="col-6 col-lg-3"><div class="card card-sm"><div class="card-body"><div class="subheader">` + label + tag + `</div><div class="h1 mb-0 mt-2 ` + h1cls + `">` + val + `</div></div></div></div>`
	}
	statusCard := func(code int, label, h1cls, badgeCls string) string {
		return statCard(label, strconv.FormatInt(s.Status[code], 10), h1cls, strconv.Itoa(code), badgeCls)
	}
	ipRows := strings.Builder{}
	for _, row := range s.TopIPs {
		ipRows.WriteString(`<tr><td>` + html.EscapeString(row.IP) + `</td><td class="text-end">` + strconv.FormatInt(row.Count, 10) + `</td></tr>`)
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
	// 所有链接从 pageQuery 单一构造器出（幂等 set，双值根杀）；空值=Del 该键（langQuery 去掉参数回落默认 zh）。
	langQuery := pageQuery(rawQuery, map[string]string{"lang": switchLang, "refresh": strconv.Itoa(refresh)})
	refreshMeta := ""
	if refresh > 0 {
		q := pageQuery(rawQuery, nil)
		refreshMeta = `<meta http-equiv="refresh" content="` + strconv.Itoa(refresh) + `;url=` + q + `">`
	}
	manualLink := pageQuery(rawQuery, nil) // 刷新按钮=原地（保当前 query）
	autoLink := pageQuery(rawQuery, map[string]string{"refresh": ""}) // 恢复自动=Del refresh（回落默认 5）——传 nil 不删键=暂停死路（对抗审 F4）
	pauseLink := pageQuery(rawQuery, map[string]string{"refresh": "0"}) // 暂停（显式 0 粘滞）
	// 刷新控制族全按钮化（单一真相：三态同款 btn-outline-secondary，无文字链混杂）
	refreshBtns := `<a href="` + manualLink + `" class="btn btn-sm btn-outline-secondary">` + c["refresh"] + `</a>`
	if refresh > 0 {
		refreshBtns += ` <a href="` + pauseLink + `" class="btn btn-sm btn-outline-secondary">` + c["autorefreshon"] + ` ⏸</a>`
	} else {
		refreshBtns += ` <a href="` + autoLink + `" class="btn btn-sm btn-outline-secondary">` + c["autorefresh"] + `</a>`
	}
	return `<!doctype html>
<html lang="` + lang + `"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">` + refreshMeta + `
<title>` + c["title"] + `</title>
<link rel="icon" type="image/png" href="/hotify-icon.png">
<link rel="stylesheet" href="/tabler.min.css">
</head><body class="theme-light">
<div class="page">
<nav class="navbar navbar-expand-md navbar-light">
  <div class="container-xl">
    <span class="navbar-brand"><img src="/hotify-icon.png" alt="" style="width:2rem;height:2rem;border-radius:.375rem;vertical-align:-.5rem"> ` + c["brand"] + `</span>
    <div class="d-flex flex-wrap gap-1 align-items-center">
      <span class="badge bg-blue-lt">` + html.EscapeString(host) + `</span>
      <span class="badge bg-secondary-lt">` + c["uptime"] + ` ` + humanDur(s.UptimeDur, lang) + `</span>
      <span class="badge bg-secondary-lt">` + c["ttl"] + ` ` + humanTTL(s.TTL, lang) + `</span>
      <span class="badge bg-lime-lt">` + mode + `</span>
      <a href="` + langQuery + `" class="btn btn-sm btn-outline-primary ms-2">` + c["switch"] + `</a>
    </div>
  </div>
</nav>
<div class="page-wrapper"><div class="page-body"><div class="container-xl">
<div class="row row-deck row-cards mb-3">` +
	statCard(c["total"], strconv.FormatInt(s.Issued, 10), "", "", "") +
	statCard(c["today"], strconv.FormatInt(s.IssuedToday, 10), "", "", "") +
	statusCard(200, c["ok"], "text-green", "bg-green-lt") +
	statusCard(401, c["auth"], "text-orange", "bg-orange-lt") +
	statusCard(403, c["ban"], "text-orange", "bg-orange-lt") +
	statusCard(429, c["limit"], "text-yellow", "bg-yellow-lt") +
	statusCard(500, c["err"], "text-red", "bg-red-lt") +
	statCard(c["autoban"], strconv.FormatInt(s.Bans, 10), "text-secondary", "", "") +
	`</div>
<div class="card card-sm mb-3">
  <div class="card-header"><h3 class="card-title">` + cardIcon("list") + c["topip"] + `</h3></div>
  <div class="card-table table-responsive"><table class="table table-vcenter">
  <thead><tr><th>` + c["iphead"] + `</th><th class="text-end">` + c["counthead"] + `</th></tr></thead>` + ipRows.String() + `</table></div>
</div>
<div class="card card-sm">
  <div class="card-header"><h3 class="card-title">` + cardIcon("terminal") + c["recent"] + `</h3><div class="card-actions">` + refreshBtns + `</div></div>
  <div class="card-body"><pre class="mb-0" style="background:#1e1e1e;color:#d4d4d4;border-radius:6px;padding:12px;font:12px/1.6 Consolas,Monaco,monospace;overflow-y:auto;max-height:300px;white-space:pre-wrap;word-break:break-all">` + logLines.String() + `</pre></div>
</div>
</div></div></div>
</div></body></html>`
}
