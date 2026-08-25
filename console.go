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

// icon 内联 stroke SVG（Tabler icons 风格，零外部依赖；UI 设计师 2026-08-25 评审定稿的全套 path）。
// 装饰性图标 aria-hidden；图标-only 按钮的语义靠调用方给 aria-label/title。
func icon(name string, size int) string {
	var path string
	switch name {
	case "list":
		path = `<path d="M9 6h11"/><path d="M9 12h11"/><path d="M9 18h11"/><path d="M5 6v.01"/><path d="M5 12v.01"/><path d="M5 18v.01"/>`
	case "terminal":
		path = `<path d="M4 17l6-6-6-6"/><path d="M12 19h8"/>`
	case "network":
		path = `<circle cx="12" cy="5.5" r="2"/><circle cx="5" cy="18.5" r="2"/><circle cx="19" cy="18.5" r="2"/><path d="M12 7.5v4M12 11.5l-5.6 5.2M12 11.5l5.6 5.2"/>`
	case "globe":
		path = `<circle cx="12" cy="12" r="9"/><path d="M3 12h18"/><path d="M12 3a15 15 0 0 1 0 18 15 15 0 0 1 0-18"/>`
	case "clock":
		path = `<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>`
	case "ticket":
		path = `<path d="M4 7a2 2 0 0 1 2-2h12a2 2 0 0 1 2 2v3a2 2 0 0 0 0 4v3a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2v-3a2 2 0 0 0 0-4z"/><path d="M14 6.5v11" stroke-dasharray="2.4 2.4"/>`
	case "lock":
		path = `<rect x="5" y="10" width="14" height="10" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/>`
	case "unlock":
		path = `<rect x="5" y="10" width="14" height="10" rx="2"/><path d="M8 10V7a4 4 0 0 1 7.7-1.5"/>`
	case "shield":
		path = `<path d="M12 3l7 3v5.5c0 4.7-3.2 8.3-7 9.5-3.8-1.2-7-4.8-7-9.5V6z"/>`
	case "refresh":
		path = `<path d="M15.5 5.94A7 7 0 1 1 18.9 10.8"/><path d="M16.4 9.1l2.5 1.7 2.5-1.7"/>`
	case "pause":
		path = `<path d="M9.5 5.5v13M14.5 5.5v13"/>`
	case "play":
		path = `<path d="M7 5.5l11 6.5-11 6.5z"/>`
	default:
		return ""
	}
	return `<svg xmlns="http://www.w3.org/2000/svg" aria-hidden="true" width="` + strconv.Itoa(size) + `" height="` + strconv.Itoa(size) + `" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" fill="none" stroke-linecap="round" stroke-linejoin="round" style="vertical-align:-.2em">` + path + `</svg>`
}

// humanCount 千分位分组（面板大数/表列共用；本面板计数恒非负）。
func humanCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) < 4 {
		return s
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		if i > 0 && (len(s)-i)%3 == 0 {
			b = append(b, ',')
		}
		b = append(b, s[i])
	}
	return string(b)
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
		"refresh": "刷新", "autorefresh": "自动刷新", "autorefreshon": "自动刷新中", "pause": "暂停自动刷新",
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
		"refresh": "Refresh", "autorefresh": "Auto refresh", "autorefreshon": "auto-refreshing", "pause": "Pause auto-refresh",
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

// consoleHTML UI 设计师 2026-08-25 激进版定稿（demo.html 同构）：
//   hero（唯一 3rem 大数=累计 + 今日 delta + meta 行收静态事实）→ 状态条（dot+label+count，
//   零计数半透明——错误不出声）→ 主舞台双栏（日志 col-lg-8 主面板 + 来源 col-lg-4）。
//   设计规则：数值全墨色（色只穿 8px dot）、图标化动作按钮（⏸ unicode 消灭）、refresh 默认开。
func consoleHTML(s statsSnapshot, lang string, host string, rawQuery string, refresh int) string {
	c := consoleCopy[lang]
	mode, modeIcon := c["anon"], "unlock"
	if s.Mode == "token-auth" {
		mode, modeIcon = c["tokenauth"], "lock"
	}
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h // 截端口（SplitHostPort 保 IPv6 [::1] 不切坏）
	}
	ipRows := strings.Builder{}
	for _, row := range s.TopIPs {
		ipRows.WriteString(`<tr><td>` + html.EscapeString(row.IP) + `</td><td class="text-end">` + humanCount(row.Count) + `</td></tr>`)
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
	// 所有链接从 pageQuery 单一构造器出（幂等 set，双值根杀）。
	langQuery := pageQuery(rawQuery, map[string]string{"lang": switchLang, "refresh": strconv.Itoa(refresh)})
	refreshMeta := ""
	if refresh > 0 {
		q := pageQuery(rawQuery, nil)
		refreshMeta = `<meta http-equiv="refresh" content="` + strconv.Itoa(refresh) + `;url=` + q + `">`
	}
	manualLink := pageQuery(rawQuery, nil)                                   // 刷新=原地
	autoLink := pageQuery(rawQuery, map[string]string{"refresh": ""})        // 恢复自动=Del refresh
	pauseLink := pageQuery(rawQuery, map[string]string{"refresh": "0"})      // 暂停（显式 0 粘滞）
	// 刷新控制族：图标按钮（aria-label 承载语义——被图标替换的词搬进来）
	spinCls := ""
	if refresh > 0 {
		spinCls = " spin" // 自动刷新态=刷新图标常转
	}
	refreshBtns := `<a href="` + manualLink + `" class="btn btn-sm btn-outline-secondary px-2" aria-label="` + c["refresh"] + `" title="` + c["refresh"] + `"><span class="icon` + spinCls + `" style="display:inline-flex">` + icon("refresh", 16) + `</span></a>`
	if refresh > 0 {
		refreshBtns += ` <span class="status-dot status-dot-animated me-1" style="background:var(--tblr-green)"></span><span class="text-secondary me-2">` + strconv.Itoa(refresh) + `s</span>` +
			`<a href="` + pauseLink + `" class="btn btn-sm btn-outline-secondary px-2" aria-label="` + c["pause"] + `" title="` + c["pause"] + `">` + icon("pause", 16) + `</a>`
	} else {
		refreshBtns += ` <a href="` + autoLink + `" class="btn btn-sm btn-outline-secondary px-2" aria-label="` + c["autorefresh"] + `" title="` + c["autorefresh"] + `">` + icon("play", 16) + `</a>`
	}
	// 状态条单元（零计数半透明）
	statCell := func(code int, label, dotColor string) string {
		cls := ""
		if s.Status[code] == 0 {
			cls = " opacity-50"
		}
		return `<div class="d-flex align-items-center gap-2` + cls + `" title="HTTP ` + strconv.Itoa(code) + `"><span class="status-dot" style="background:var(--tblr-` + dotColor + `)"></span><span class="text-secondary">` + label + `</span><span style="font-size:1.125rem;font-weight:600">` + humanCount(s.Status[code]) + `</span></div>`
	}
	return `<!doctype html>
<html lang="` + lang + `"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">` + refreshMeta + `
<title>` + c["title"] + `</title>
<link rel="icon" type="image/png" href="/hotify-icon.png">
<link rel="stylesheet" href="/tabler.min.css">
<style>
.card{transition:border-color .15s ease,box-shadow .15s ease}
.card:hover{border-color:rgba(6,111,209,.28);box-shadow:0 1px 8px rgba(0,0,0,.05)}
a.btn{transition:transform .08s ease}
a.btn:active{transform:scale(.96)}
a.btn:focus-visible{outline:2px solid #066fd1;outline-offset:2px}
.spin{animation:console-spin 1.8s linear infinite;transform-origin:50% 50%}
@keyframes console-spin{to{transform:rotate(360deg)}}
body{animation:console-fade .18s ease-out}
@keyframes console-fade{from{opacity:.55}}
@media (prefers-reduced-motion:reduce){.spin{animation:none}body{animation:none}.status-dot-animated{animation:none}}
</style>
</head><body class="theme-light">
<div class="page">
<nav class="navbar navbar-expand-md navbar-light">
  <div class="container-xl">
    <span class="navbar-brand"><img src="/hotify-icon.png" alt="" style="width:2rem;height:2rem;border-radius:.375rem;vertical-align:-.5rem"> ` + c["brand"] + `</span>
    <div class="d-flex align-items-center gap-2">
      <span class="badge bg-lime-lt">` + icon(modeIcon, 14) + ` ` + mode + `</span>
      <a href="` + langQuery + `" class="btn btn-sm btn-outline-primary ms-2">` + c["switch"] + `</a>
    </div>
  </div>
</nav>
<div class="page-wrapper"><div class="page-body"><div class="container-xl">

<div class="card mb-3"><div class="card-body">
  <div class="d-flex flex-wrap align-items-end justify-content-between gap-3">
    <div>
      <div class="subheader">` + c["total"] + `</div>
      <div style="font-size:3rem;font-weight:600;line-height:1.05">` + humanCount(s.Issued) + `</div>
    </div>
    <div class="text-end">
      <div class="subheader">` + c["today"] + `</div>
      <div class="h2 mb-0">+` + humanCount(s.IssuedToday) + `</div>
    </div>
  </div>
  <div class="hr my-3"></div>
  <div class="d-flex flex-wrap text-secondary gap-3" style="font-size:.875rem">
    <span>` + icon("globe", 14) + ` ` + html.EscapeString(host) + `</span>
    <span>` + icon("clock", 14) + ` ` + c["uptime"] + ` ` + humanDur(s.UptimeDur, lang) + `</span>
    <span>` + icon("ticket", 14) + ` ` + c["ttl"] + ` ` + humanTTL(s.TTL, lang) + `</span>
    <span>` + icon("terminal", 14) + ` ` + c["project"] + ` ` + maskProject(s.ProjectID) + `</span>
  </div>
</div></div>

<div class="card mb-3"><div class="card-body py-3">
  <div class="d-flex flex-wrap align-items-center gap-3">` +
	statCell(401, c["auth"], "orange") +
	statCell(403, c["ban"], "orange") +
	statCell(429, c["limit"], "yellow") +
	statCell(500, c["err"], "red") +
	`<div style="width:1px;align-self:stretch;background:var(--tblr-border-color)"></div>
    <div class="d-flex align-items-center gap-2" title="auto-ban">` + icon("shield", 16) + `<span class="text-secondary">` + c["autoban"] + `</span><span style="font-size:1.125rem;font-weight:600">` + humanCount(s.Bans) + `</span></div>
  </div>
</div></div>

<div class="row row-deck row-cards mb-3">
  <div class="col-lg-8">
    <div class="card card-sm">
      <div class="card-header">
        <h3 class="card-title">` + icon("terminal", 24) + ` ` + c["recent"] + `</h3>
        <div class="card-actions d-flex align-items-center gap-2">` + refreshBtns + `</div>
      </div>
      <div class="card-body">
        <pre class="mb-0" style="background:#1e1e1e;color:#d4d4d4;border-radius:6px;padding:12px;font:12px/1.6 Consolas,Monaco,monospace;overflow-y:auto;max-height:clamp(240px,45vh,560px);white-space:pre-wrap;overflow-wrap:anywhere">` + logLines.String() + `</pre>
      </div>
    </div>
  </div>
  <div class="col-lg-4">
    <div class="card card-sm">
      <div class="card-header"><h3 class="card-title">` + icon("network", 24) + ` ` + c["topip"] + `</h3></div>
      <div class="card-table table-responsive">
        <table class="table table-vcenter table-hover">
          <thead><tr><th>` + c["iphead"] + `</th><th class="text-end">` + c["counthead"] + `</th></tr></thead>
          <tbody style="font-variant-numeric:tabular-nums">` + ipRows.String() + `</tbody>
        </table>
      </div>
    </div>
  </div>
</div>

</div></div></div>
</div></body></html>`
}
