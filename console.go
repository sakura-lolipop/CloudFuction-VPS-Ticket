package main

// /console 观察面板（2026-08-25 加，stats 单独 CP）：内存计数 + 最近日志 ring + 单页 HTML。
// 样式 = NEXT-Server /console 同款 Tabler dist（web/tabler.min.css go:embed，二进制自包含
// 不引 CDN——联邦件零外依赖）；显示层标记用 ASCII（OK/ERR/BAN/LIMIT，避免字形缺字体豆腐块）。
// 免鉴权直接进（观察数据不敏感，自己节点的量/IP/状态码）；JSON 模式 ?json=1。
// 数据单锁粗粒度（签票低频，不存在的竞争不设计）。

import (
	_ "embed"
	"encoding/json"
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
	status      map[int]int64    // 状态码 → 次数
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
	UptimeSec   int64          `json:"uptime_sec"` // 面板 JS 增量刷人性化用(2026-08-30;Uptime 字符串不动=契约不破坏)
	UptimeDur   time.Duration  `json:"-"`          // 面板人性化用（humanDur）
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
		UptimeSec:   int64(nowFn().Sub(stats.started).Truncate(time.Second).Seconds()),
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
	case "refresh": // reload 官方形(2026-08-30 用户裁定换:旧 refresh 断箭尾不好看;Tabler 官方源取 path)
		path = `<path d="M19.933 13.041a8 8 0 1 1 -9.925 -8.788c3.899 -1 7.935 1.007 9.425 4.747"/><path d="M20 4v5h-5"/>`
	case "pause":
		path = `<path d="M9.5 5.5v13M14.5 5.5v13"/>`
	case "play":
		path = `<path d="M7 5.5l11 6.5-11 6.5z"/>`
	// 胶囊三件套+主题双态(2026-08-30 lookinggood):path 取自 NEXT-Server webui/icons(Tabler 源文件)
	case "language":
		path = `<path d="M9 6.371c0 4.418 -2.239 6.629 -5 6.629"/><path d="M4 6.371h7"/><path d="M5 9c0 2.144 2.252 3.908 6 4"/><path d="M12 20l4 -9l4 9"/><path d="M19.1 18h-6.2"/><path d="M6.694 3l.793 .582"/>`
	case "moon-stars":
		path = `<path d="M12 3c.132 0 .263 0 .393 0a7.5 7.5 0 0 0 7.92 12.446a9 9 0 1 1 -8.313 -12.454l0 .008"/><path d="M17 4a2 2 0 0 0 2 2a2 2 0 0 0 -2 2a2 2 0 0 0 -2 -2a2 2 0 0 0 2 -2"/><path d="M19 11h2m-1 -1v2"/>`
	case "sun-high":
		path = `<path d="M14.828 14.828a4 4 0 1 0 -5.656 -5.656a4 4 0 0 0 5.656 5.656"/><path d="M6.343 17.657l-1.414 1.414"/><path d="M6.343 6.343l-1.414 -1.414"/><path d="M17.657 6.343l1.414 -1.414"/><path d="M17.657 17.657l1.414 1.414"/><path d="M4 12h-2"/><path d="M12 4v-2"/><path d="M20 12h2"/><path d="M12 20v2"/>`
	case "photo":
		path = `<path d="M15 8h.01"/><path d="M3 6a3 3 0 0 1 3 -3h12a3 3 0 0 1 3 3v12a3 3 0 0 1 -3 3h-12a3 3 0 0 1 -3 -3v-12"/><path d="M3 16l5 -5c.928 -.893 2.072 -.893 3 0l5 5"/><path d="M14 14l1 -1c.928 -.893 2.072 -.893 3 0l3 3"/>`
	case "palette":
		path = `<path d="M12 21a9 9 0 0 1 0 -18c4.97 0 9 3.582 9 8c0 1.06 -.474 2.078 -1.318 2.828c-.844 .75 -1.989 1.172 -3.182 1.172h-2.5a2 2 0 0 0 -1 3.75a1.3 1.3 0 0 1 -1 2.25z"/><path d="M8.5 10.5m-1 0a1 1 0 1 0 2 0a1 1 0 1 0 -2 0"/><path d="M12.5 7.5m-1 0a1 1 0 1 0 2 0a1 1 0 1 0 -2 0"/><path d="M16.5 10.5m-1 0a1 1 0 1 0 2 0a1 1 0 1 0 -2 0"/>`
	case "x":
		path = `<path d="M18 6l-12 12"/><path d="M6 6l12 12"/>`
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
		"topip":   "签发来源", "recent": "实时日志",
		"iphead": "IP", "counthead": "签发次数",
		"refresh": "刷新", "autorefresh": "自动刷新", "autorefreshon": "自动刷新中", "pause": "暂停自动刷新",
		"empty": "暂无签发", "switch": "English",
		// 胶囊+光感面板(2026-08-30 lookinggood)
		"theme": "主题", "bgshow": "显示壁纸", "bghide": "隐藏壁纸",
		"lptitle": "光感设置", "gain": "整体强度", "edge": "边缘强度", "radius": "光晕大小",
		"ds": "卡面补偿", "color": "颜色",
		"blob": "光晕显示", "reset": "恢复默认", "themedark": "深色", "themelight": "浅色",
	},
	"en": {
		"title": "Hotify Ticket", "brand": "Hotify Ticket",
		"uptime": "uptime", "ttl": "ttl", "anon": "anonymous", "tokenauth": "Token auth", "project": "project",
		"total": "total issued", "today": "today",
		"ok": "Success", "auth": "Unauthorized", "ban": "Banned", "limit": "Rate limited", "err": "Server error",
		"autoban": "auto-ban",
		"topip":   "Top sources", "recent": "Live log",
		"iphead": "IP", "counthead": "tickets",
		"refresh": "Refresh", "autorefresh": "Auto refresh", "autorefreshon": "auto-refreshing", "pause": "Pause auto-refresh",
		"empty": "no tickets yet", "switch": "中文",
		"theme": "Theme", "bgshow": "Show wallpaper", "bghide": "Hide wallpaper",
		"lptitle": "Light effects", "gain": "Overall", "edge": "Edges", "radius": "Glow size",
		"ds": "Card boost", "color": "Color",
		"blob": "Show glow", "reset": "Reset", "themedark": "Dark", "themelight": "Light",
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
	// 轮询间隔(秒):?refresh=N 显式给则从 URL(0=进页即暂停),否则 5s。
	// 2026-08-30 起不再是 meta 整页刷新——仅作 JS 轮询初始档;暂停粘滞见 JS localStorage。
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

// consoleHTML 2026-08-30 lookinggood 改版（原版=UI 设计师 2026-08-25 激进版，IA 继承不动）：
//
//	去堆砌：navbar 横条拆 → 品牌裸浮+右上悬浮玻璃胶囊（语言/主题/壁纸/调色板/刷新控制）；
//	hero 卡吞并状态码条（5 面→3 面：hero / 日志 / 来源）。
//	meta 整页刷新 → fetch ?json=1 轮询 + keyed DOM 增量（数值变色闪/日志 prepend 滑入/TopIP 变才重排）。
//	玻璃分层照 NEXT-Server 裁定：卡=纯透零 blur+顶缘高光；胶囊/面板=blur(12/16px)+saturate(160%)。
//	光晕=web-immersive-light CSS 四通道内联；调色板面板分主题记忆（localStorage）。
//	主题：data-bs-theme dark/light，FOUC 由 head 内联脚本抢在 CSS 前钉属性；默认跟系统。
//	<!--DEMOMOCK--> 是 demo 生成器（demogen_test.go）的注入点，生产渲染为惰性注释。
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
		// data-ip= keyed 增量更新的行键（JS syncIPs 按它原地改数不重建）
		ipRows.WriteString(`<tr data-ip="` + html.EscapeString(row.IP) + `"><td>` + html.EscapeString(row.IP) + `</td><td class="text-end">` + humanCount(row.Count) + `</td></tr>`)
	}
	if ipRows.Len() == 0 {
		ipRows.WriteString(`<tr><td colspan="2" class="text-secondary">` + c["empty"] + `</td></tr>`)
	}
	logLines := strings.Builder{}
	for _, ln := range s.Recent {
		// 单行一 div=增量 prepend 的插入单元（row-in 入场动画挂 div）
		logLines.WriteString(`<div>` + html.EscapeString(asciiize(ln)) + `</div>`)
	}
	switchLang := "en"
	if lang == "en" {
		switchLang = "zh"
	}
	// 语言切换链接保留全部现有 query——pageQuery 幂等 set，双值根杀
	langQuery := pageQuery(rawQuery, map[string]string{"lang": switchLang})
	// JS 侧文案（轮询增量用）：CJ 一处注入，键名单一在 consoleCopy
	cjKeys := []string{"pause", "autorefresh", "theme", "bgshow", "bghide", "lptitle", "gain", "edge",
		"radius", "ds", "color", "blob", "reset", "themedark", "themelight", "empty", "uptime"}
	cjMap := map[string]string{}
	for _, k := range cjKeys {
		cjMap[k] = c[k]
	}
	cjJSON, _ := json.Marshal(cjMap)
	// ?refresh 显式给了（URL）则压过 localStorage 暂停记忆；没给则 JS 读 localStorage
	refreshURLSet := parseRawQuery(rawQuery).Get("refresh") != ""
	urlSet := "false"
	if refreshURLSet {
		urlSet = "true"
	}
	// 状态条单元（零计数半透明=错误不出声；id 供 JS 增量写）
	statCell := func(code int, label, dotColor string) string {
		cls := ""
		if s.Status[code] == 0 {
			cls = " dim"
		}
		return `<div class="sc` + cls + `" id="sc-` + strconv.Itoa(code) + `" title="HTTP ` + strconv.Itoa(code) + `"><span class="status-dot" style="background:var(--tblr-` + dotColor + `)"></span><span class="text-secondary">` + label + `</span><span class="n" id="sc-` + strconv.Itoa(code) + `-n">` + humanCount(s.Status[code]) + `</span></div>`
	}
	banCls := ""
	if s.Bans == 0 {
		banCls = " dim"
	}
	return `<!doctype html>
<html lang="` + lang + `"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>` + c["title"] + `</title>
<link rel="icon" type="image/png" href="/hotify-icon.png">
<script>(function(){var t=null;try{t=localStorage.getItem('hotify-theme')}catch(e){}
if(!t)t=(window.matchMedia&&matchMedia('(prefers-color-scheme: dark)').matches)?'dark':'light';
document.documentElement.setAttribute('data-bs-theme',t)})()</script>
<link rel="stylesheet" href="/tabler.min.css">
<style>
/* 玻璃+光感配方单一真相（:root=浅色档，dark 覆盖；消费方只引用不写配方） */
:root{
  --chrome-face:rgba(255,255,255,.68);
  --card-face:88%;
  --scrim:rgba(246,246,242,.78);
  --line:rgba(11,11,11,.10);
  --glass-chrome:blur(12px) saturate(160%);
  --glass-modal:blur(16px) saturate(160%);
  /* 光感配方（server console.css 同构 2026-08-30 对齐）：base 裸数 JS canvas 读自乘 gain；
     --light-a/-edge-falloff/-edge-scale 中间变量已随 Canvas 化退役，edge 衰减硬编进 JS */
  --light-r:80px;--light-falloff:65%;
  --light-a-base:.08; /* 光球/面斑基准（浅色档=server [data-bs-theme=light] 同值） */
  --light-gain:1; /* 增益总闸（面板写 inline，分主题各自生效） */
  --light-edge-base:.20;--light-edge-a:calc(var(--light-edge-base) * var(--light-gain));
  --light-dsurface:.03; /* 卡面补偿 Δ（on-surface calc(base×gain+Δ)） */
  --light-rgb:var(--tblr-primary-rgb); /* 受光色独立（var 字面 JS 读不出=JS fallback 链回落，server 同款） */
  --light-row-r:36px; /* CF2 本地档：密集行(TopIP 45px/行)小半径——server 无此类密集表行，防多行同亮错乱 */
  --light-blend:normal   /* 浅色：screen(白,x)=白 数学不可见 */
}
html[data-bs-theme=dark]{
  --chrome-face:rgba(25,33,48,.55);
  --card-face:92%;
  --scrim:rgba(10,10,26,.6);
  --line:rgba(255,255,255,.09);
  --light-a-base:.13;
  --light-edge-base:.32; /* CF2 校准(2026-08-30 蓝壁纸+蓝受光同色相 .20 洗没；server 两档同 .20) */
  --light-blend:screen
}
/* 壁纸层（05-bing 同构）：照片+纱；关=同 opacity 过渡淡出（不硬切） */
#bg-photo{position:fixed;inset:0;z-index:-2;background-size:cover;background-position:center;
  opacity:0;transition:opacity 2s ease-in;filter:saturate(.85)}
#bg-photo.loaded{opacity:1}
#bg-scrim{position:fixed;inset:0;z-index:-1;background:var(--scrim);pointer-events:none;transition:opacity 2s ease-in}
/* 两层同时长同曲线（server 校准：异曲线中途有裸照窗；主题切换动画走 VT 快照——曾试 scrim
   色补间+全页通配 transition，用户两轮报撕裂感，server 已删不搬） */
#bg-photo.bg-off,#bg-scrim.bg-off{opacity:0}
.wrap{max-width:72rem;margin:0 auto;padding:18px 16px 64px}
.topbar{display:flex;align-items:center;justify-content:space-between;gap:12px;margin-bottom:14px;flex-wrap:wrap}
.brand{display:flex;align-items:center;gap:10px;font-size:1.1rem;font-weight:700}
.brand img{width:2rem;height:2rem;border-radius:.375rem}
.hostpill{font:11px ui-monospace,Consolas,monospace;font-weight:400;color:var(--tblr-secondary);
  border:1px solid var(--line);border-radius:999px;padding:2px 10px}
/* 悬浮玻璃胶囊（navbar 横条拆除后的动作家） */
.capsule{display:flex;align-items:center;gap:2px;background:var(--chrome-face);
  border:1px solid var(--line);border-radius:999px;padding:4px;
  -webkit-backdrop-filter:var(--glass-chrome);backdrop-filter:var(--glass-chrome)}
.cap-btn{border:0;background:transparent;color:inherit;width:36px;height:36px;border-radius:50%;
  display:inline-flex;align-items:center;justify-content:center;cursor:pointer;padding:0;text-decoration:none}
.cap-btn:hover{background:rgba(127,143,166,.22)}
.cap-sep{width:1px;align-self:stretch;margin:4px 2px;background:var(--line)}
@media (pointer:coarse){.cap-btn{width:44px;height:44px}}
/* 正文卡=纯透零 blur+顶缘高光（厚度感；长滚动面 backdrop-filter 是重绘成本，NEXT-Server 裁定） */
.card{border:1px solid var(--line);
  background-color:color-mix(in srgb, var(--tblr-bg-surface) var(--card-face), transparent);
  box-shadow:inset 0 1px 0 rgba(255,255,255,var(--light-edge-a)), var(--tblr-card-box-shadow)}
.hero-big{font-size:3rem;font-weight:600;line-height:1.05;font-variant-numeric:tabular-nums}
.sc{display:flex;align-items:center;gap:.5rem}
.sc .n{font-size:1.125rem;font-weight:600;font-variant-numeric:tabular-nums}
.dim{opacity:.45}
.meta{display:flex;flex-wrap:wrap;gap:.4rem 1.1rem;color:var(--tblr-secondary);font-size:.875rem}
.flash{animation:cflash .5s ease-out}
@keyframes cflash{from{color:var(--tblr-primary)}}
.logbox{background:#141414;color:#c9c9c2;border-radius:.5rem;padding:12px;
  font:12px/1.7 ui-monospace,Consolas,Monaco,monospace;overflow-y:auto;max-height:clamp(240px,45vh,560px)}
.logbox div{white-space:pre-wrap;overflow-wrap:anywhere}
.row-in{animation:rowIn .25s ease-out}
@keyframes rowIn{from{opacity:0;transform:translateY(-5px)}}
.is-off{display:none!important}
input[type=range]{accent-color:var(--tblr-primary)}
/* 光感面板（server #light-panel 同构）：弹层玻璃档；桌面 JS 锚钮下（anchorPanel 居中+视口 clamp），
   窄屏底部抽屉；translateZ+will-change=独立合成层（防玻璃重采样闪烁，server W13 同款） */
#light-panel{position:fixed;z-index:1060;width:19.5rem;max-width:calc(100vw - 1rem);
  background:color-mix(in srgb, var(--tblr-bg-surface) 88%, transparent);
  -webkit-backdrop-filter:var(--glass-modal);backdrop-filter:var(--glass-modal);
  border:1px solid var(--line);border-radius:.75rem;padding:.875rem;transform:translateZ(0);
  will-change:top,left,backdrop-filter}
.lp-head{display:flex;justify-content:space-between;align-items:center;margin-bottom:.5rem;font-weight:600}
.lp-close{border:0;background:transparent;color:inherit;cursor:pointer;padding:0 4px;display:inline-flex}
.lp-row{display:grid;grid-template-columns:4.5rem 1fr 3.5rem;align-items:center;gap:.5rem;margin-bottom:.5rem}
.lp-row label{font-size:.8125rem;margin:0}
.lp-row output{text-align:right;font-size:.8125rem;color:var(--tblr-secondary);font-variant-numeric:tabular-nums}
.lp-sw-row{display:flex;align-items:center;gap:.375rem;flex-wrap:wrap}
#lp-swatches{display:flex;gap:.375rem;flex-wrap:wrap}
.lp-sw{width:1.375rem;height:1.375rem;border-radius:50%;border:2px solid transparent;cursor:pointer;padding:0}
.lp-sw.on{border-color:var(--tblr-body-color)}
#lp-picker{width:1.75rem;height:1.75rem;padding:0;border:0;background:transparent;cursor:pointer}
#lp-hex{width:6.5rem}
.lp-foot{display:flex;align-items:center;justify-content:space-between;margin-top:.625rem;font-size:.8125rem;color:var(--tblr-secondary)}
@media (max-width:767.98px){#light-panel{top:auto!important;bottom:.5rem;left:.5rem;right:.5rem;width:auto}}
@keyframes viewIn{from{opacity:0;transform:translateY(4px)}to{opacity:1;transform:none}}
.view-in{animation:viewIn .18s ease-out}
/* 主题切换动画：View Transitions 圆形揭示（server 2026-09-01 终态同构：新态从点击处扩圆盖掉
   旧态，双向统一扩圆——入暗收圆曾有「被吸向顶栏」观感弃用；动画住 CSS keyframes 生命周期浏览器
   托管，坐标经 --vt-x/y/r 由 JS 写入——WAAPI 持 VT 伪元素引用=Edge 狂点 UAF 崩，已根治） */
::view-transition-old(root),::view-transition-new(root){animation:none;mix-blend-mode:normal}
::view-transition-new(root){
  z-index:9999;
  animation:vt-circle .3s ease-out both}
@keyframes vt-circle{
  from{clip-path:circle(0px at var(--vt-x,50%) var(--vt-y,50%))}
  to{clip-path:circle(var(--vt-r,150vmax) at var(--vt-x,50%) var(--vt-y,50%))}}
@media (prefers-reduced-motion:reduce){.row-in,.flash,.view-in{animation:none!important}
  ::view-transition-group(*),::view-transition-old(*),::view-transition-new(*){animation:none!important}
  #bg-photo,#bg-scrim{transition:none!important}}
/* ── 沉浸光感·全局 Canvas 层（server 15-light.js 同构 2026-08-30 对齐）：四通道渲染唯一在
   JS 绘制循环画进 #light-canvas；.lit/.lit-row 纯注册标记（CSS 零受光渲染）；z 随域切
   （页面域 1049=#light-panel(1060) 天然挡光，server 2026-09-01 光域 CP 同款）── */
#light-canvas{position:fixed;left:0;top:0;z-index:1049;pointer-events:none;
  mix-blend-mode:var(--light-blend)} /* 尺寸/DPR/绘制全在 JS;初始=页面域 1049 */
html.noblend #light-canvas{mix-blend-mode:normal} /* ?light=noblend 消融（server 同款自证通道） */
/* input 受光主体=边缘（server 同款）：凹面上缘投影+内周微高光；focus 环走 Tabler 原生 */
.form-control{box-shadow:inset 0 1px 3px rgba(0,0,0,.15), inset 0 0 0 1px rgba(255,255,255,.07)}
[data-bs-theme=light] .form-control:focus{box-shadow:inset 0 1px 3px rgba(0,0,0,.15), inset 0 0 0 1px rgba(255,255,255,.07),
  0 0 0 .15rem rgba(var(--light-rgb), calc(var(--light-edge-a) - .02))}
</style>
</head><body>
<div id="bg-photo"></div><div id="bg-scrim"></div>
<div class="wrap">
<header class="topbar">
  <span class="brand"><img src="/hotify-icon.png" alt=""> ` + c["brand"] + ` <span class="hostpill">` + html.EscapeString(host) + `</span></span>
  <div class="capsule">
    <a href="` + langQuery + `" class="cap-btn" title="` + c["switch"] + `" aria-label="` + c["switch"] + `">` + icon("language", 18) + `</a>
    <button id="btn-theme" class="cap-btn" title="` + c["theme"] + `" aria-label="` + c["theme"] + `"><span id="ic-moon">` + icon("moon-stars", 18) + `</span><span id="ic-sun" class="is-off">` + icon("sun-high", 18) + `</span></button>
    <button id="btn-bg" class="cap-btn" title="` + c["bghide"] + `" aria-label="` + c["bghide"] + `" aria-pressed="false">` + icon("photo", 18) + `</button>
    <button id="btn-palette" class="cap-btn" title="` + c["lptitle"] + `" aria-label="` + c["lptitle"] + `">` + icon("palette", 18) + `</button>
    <span class="cap-sep"></span>
    <button id="btn-ref" class="cap-btn" title="` + c["refresh"] + `" aria-label="` + c["refresh"] + `"><span id="ic-ref" style="display:inline-flex">` + icon("refresh", 18) + `</span></button>
    <button id="btn-pause" class="cap-btn" title="` + c["pause"] + `" aria-label="` + c["pause"] + `"><span id="ic-pause">` + icon("pause", 18) + `</span><span id="ic-play" class="is-off">` + icon("play", 18) + `</span></button>
  </div>
</header>

<div class="card mb-3"><div class="card-body">
  <div class="d-flex flex-wrap align-items-end justify-content-between gap-3">
    <div>
      <div class="subheader">` + c["total"] + `</div>
      <div class="hero-big" id="v-total">` + humanCount(s.Issued) + `</div>
    </div>
    <div class="d-flex align-items-center gap-3">
      <span class="badge bg-lime-lt">` + icon(modeIcon, 14) + ` ` + mode + `</span>
      <div class="text-end">
        <div class="subheader">` + c["today"] + `</div>
        <div class="h2 mb-0" id="v-today">+` + humanCount(s.IssuedToday) + `</div>
      </div>
    </div>
  </div>
  <div class="hr my-3"></div>
  <div class="d-flex flex-wrap align-items-center gap-3 mb-3">` +
		statCell(401, c["auth"], "orange") +
		statCell(403, c["ban"], "orange") +
		statCell(429, c["limit"], "yellow") +
		statCell(500, c["err"], "red") +
		`<div class="cap-sep"></div>
    <div class="sc` + banCls + `" id="sc-ban" title="auto-ban">` + icon("shield", 16) + `<span class="text-secondary">` + c["autoban"] + `</span><span class="n" id="sc-ban-n">` + humanCount(s.Bans) + `</span></div>
  </div>
  <div class="meta">
    <span>` + icon("globe", 14) + ` ` + html.EscapeString(host) + `</span>
    <span>` + icon("clock", 14) + ` <span id="v-uptime" data-sec="` + strconv.FormatInt(s.UptimeSec, 10) + `">` + c["uptime"] + ` ` + humanDur(s.UptimeDur, lang) + `</span></span>
    <span>` + icon("ticket", 14) + ` ` + c["ttl"] + ` ` + humanTTL(s.TTL, lang) + `</span>
    <span>` + icon("terminal", 14) + ` ` + c["project"] + ` ` + maskProject(s.ProjectID) + `</span>
  </div>
</div></div>

<div class="row row-deck row-cards mb-3">
  <div class="col-lg-8">
    <div class="card card-sm">
      <div class="card-header">
        <h3 class="card-title">` + icon("terminal", 24) + ` ` + c["recent"] + `</h3>
      </div>
      <div class="card-body">
        <div class="logbox" id="log" role="log">` + logLines.String() + `</div>
      </div>
    </div>
  </div>
  <div class="col-lg-4">
    <div class="card card-sm">
      <div class="card-header"><h3 class="card-title">` + icon("network", 24) + ` ` + c["topip"] + `</h3></div>
      <div class="card-table table-responsive">
        <table class="table table-vcenter table-hover">
          <thead><tr><th>` + c["iphead"] + `</th><th class="text-end">` + c["counthead"] + `</th></tr></thead>
          <tbody id="ipbody" style="font-variant-numeric:tabular-nums">` + ipRows.String() + `</tbody>
        </table>
      </div>
    </div>
  </div>
</div>
</div>
<canvas id="light-canvas"></canvas>
<div id="light-panel" class="is-off" role="dialog" aria-label="light settings">
  <div class="lp-head"><span id="lp-title">` + c["lptitle"] + `</span><button id="lp-close" class="lp-close" aria-label="close">` + icon("x", 16) + `</button></div>
  <div class="lp-row"><label for="lp-gain">` + c["gain"] + `</label><input type="range" id="lp-gain" min="0" max="2" step="0.05"><output id="lp-gain-v"></output></div>
  <div class="lp-row"><label for="lp-edge">` + c["edge"] + `</label><input type="range" id="lp-edge" min="0.05" max="0.6" step="0.01"><output id="lp-edge-v"></output></div>
  <div class="lp-row"><label for="lp-r">` + c["radius"] + `</label><input type="range" id="lp-r" min="40" max="160" step="5"><output id="lp-r-v"></output></div>
  <div class="lp-row"><label for="lp-ds">` + c["ds"] + `</label><input type="range" id="lp-ds" min="0" max="0.15" step="0.01"><output id="lp-ds-v"></output></div>
  <div class="lp-row" style="grid-template-columns:4.5rem 1fr auto;margin-bottom:.25rem">
    <label for="lp-blob">` + c["blob"] + `</label><span></span>
    <div class="form-check form-switch m-0"><!-- Tabler 原生滑块开关（server 同款） -->
      <input type="checkbox" id="lp-blob" class="form-check-input" checked><!-- 只关光球,面斑/边缘带照常 -->
    </div>
  </div>
  <div class="lp-sw-row" style="margin-bottom:.25rem"><span class="text-secondary" style="font-size:.8125rem">` + c["color"] + `</span></div>
  <div class="lp-sw-row"><span id="lp-swatches"></span>
    <input type="color" id="lp-picker" title="picker" aria-label="color picker"><input type="text" id="lp-hex" class="form-control form-control-sm font-monospace" placeholder="#066fd1" autocomplete="off" spellcheck="false"></div>
  <div class="lp-foot"><span id="lp-hint"></span><button id="lp-reset" class="btn btn-sm btn-outline-secondary">` + c["reset"] + `</button></div>
</div>
<!--DEMOMOCK-->
<script>
var CJ=` + string(cjJSON) + `;CJ.zh=` + strconv.FormatBool(lang == "zh") + `;
var REFRESH=` + strconv.Itoa(refresh) + `,REFRESH_URL=` + urlSet + `;
(function(){
"use strict";
var $=function(i){return document.getElementById(i)};
/* ── 主题（FOUC 已在 head 抢钉；此处只管切换+图标）── */
function curTheme(){return document.documentElement.getAttribute('data-bs-theme')==='dark'?'dark':'light'}
function paintTheme(){var d=curTheme()==='dark';$('ic-moon').classList.toggle('is-off',d);$('ic-sun').classList.toggle('is-off',!d)}
var themeVTBusy=false;
$('btn-theme').addEventListener('click',function(ev){
  if(themeVTBusy)return;
  var h=document.documentElement,dark=curTheme()==='dark';
  var apply=function(){
    h.setAttribute('data-bs-theme',dark?'light':'dark');
    try{localStorage.setItem('hotify-theme',dark?'light':'dark')}catch(e){}
    paintTheme();
    if(window.CF2ThemeApplied)window.CF2ThemeApplied() /* attr 真翻转后回调（CF2 修正：server 版
      setTimeout(0) 第二监听在 VT 下先于 apply 跑=读旧主题，潜伏错一档） */
  };
  if(!document.startViewTransition||
     (window.matchMedia&&matchMedia('(prefers-reduced-motion: reduce)').matches)){apply();return}
  /* tap 坐标减 visualViewport 偏移（地址栏展开时 VT 快照锚 layout 空间，不减圆心画进地址栏后） */
  var st=h.style,vv=window.visualViewport;
  var vx=(ev.clientX||innerWidth/2)-(vv?vv.offsetLeft:0);
  var vy=(ev.clientY||innerHeight/2)-(vv?vv.offsetTop:0);
  st.setProperty('--vt-x',vx+'px');
  st.setProperty('--vt-y',vy+'px');
  st.setProperty('--vt-r',Math.hypot(Math.max(vx,innerWidth-vx),Math.max(vy,innerHeight-vy))+'px');
  if(/[?&]vtdebug=1/.test(location.search)){ /* 真机自诊通道（?light= 同惯例；server 走 core toast，
    CF2 无 toast 基建=最小 fixed div 变体） */
    var br=this.getBoundingClientRect();
    setTimeout(function(){
      var d=document.createElement('div');
      d.textContent='vtdebug tap('+Math.round(ev.clientX)+','+Math.round(ev.clientY)+') btn('+
        Math.round(br.x)+','+Math.round(br.y)+','+Math.round(br.width)+'x'+Math.round(br.height)+') vv('+
        (window.visualViewport?Math.round(visualViewport.scale*100)/100+','+Math.round(visualViewport.offsetLeft)+','+Math.round(visualViewport.offsetTop):'n/a')+')';
      d.style.cssText='position:fixed;left:8px;top:8px;z-index:3000;font:11px/1.5 monospace;'+
        'background:rgba(0,0,0,.75);color:#8fd;padding:6px 8px;border-radius:6px;max-width:90vw';
      document.body.appendChild(d);
      setTimeout(function(){d.remove()},4000);
    },320);
  }
  themeVTBusy=true;
  document.startViewTransition(apply).finished.finally(function(){
    themeVTBusy=false;
    st.removeProperty('--vt-x');st.removeProperty('--vt-y');st.removeProperty('--vt-r');
  });
});
paintTheme();
/* ── 壁纸（05-bing 移植：旧图先行/当日缓存/朝向派生裁切/开关 opacity 过渡）── */
var bingUrl='';
function bingSwap(url,suffix){
  if(/_(UHD|\d+x\d+)\.jpg/i.test(url))return url.replace(/_(UHD|\d+x\d+)\.jpg/i,suffix);
  return url.replace(/\.jpg$/i,suffix);
}
function bingApply(){
  if(!bingUrl)return;
  var suffix=window.matchMedia('(orientation: portrait)').matches?'_768x1280.jpg':'_UHD.jpg';
  var bg=$('bg-photo');bg.style.backgroundImage='url('+bingSwap(bingUrl,suffix)+')';bg.classList.add('loaded');
}
(function loadBing(){
  var today=new Date().toISOString().slice(0,10);
  try{var cached=JSON.parse(localStorage.getItem('hotify-bing-bg3'));
    if(cached&&cached.url){bingUrl=cached.url;bingApply();if(cached.date===today)return}}catch(e){}
  fetch('https://bing.biturl.top/?resolution=1920&format=json&index=0&mkt=zh-CN')
    .then(function(r){return r.ok?r.json():null})
    .then(function(j){var url=j&&((j.data&&j.data.url)||j.url)||null;if(!url)throw 0;return url})
    .catch(function(){return fetch('https://api.oioweb.cn/api/bing')
      .then(function(r){return r.ok?r.json():null})
      .then(function(j){return j&&((j.data&&j.data.url)||(j.result&&j.result[0]&&j.result[0].url))||null})})
    .then(function(url){
      if(!url||!/^https?:\/\//i.test(url))return;
      bingUrl=url;bingApply();
      try{localStorage.setItem('hotify-bing-bg3',JSON.stringify({date:today,url:url}))}catch(e){}
    }).catch(function(){});
})();
var mq=window.matchMedia('(orientation: portrait)');
if(mq.addEventListener)mq.addEventListener('change',bingApply);
/* 视口高锁（server 05-bing 同款）：lvh 双写在无 lvh 内核回落 vh 仍随键盘缩——JS 记录最大高
   只增不减（旋转重置）写 --vp-h，壁纸/纱/光感 canvas 统一消费这一个 writer */
(function(){
  var maxW=window.innerWidth,maxH=window.innerHeight;
  function lockVH(){
    if(window.innerWidth!==maxW){maxW=window.innerWidth;maxH=window.innerHeight}
    else if(window.innerHeight>maxH)maxH=window.innerHeight;
    document.documentElement.style.setProperty('--vp-h',maxH+'px');
  }
  window.addEventListener('resize',lockVH);
  lockVH();
})();
function bgOff(){try{return localStorage.getItem('hotify-bg-off')==='1'}catch(e){return false}}
function paintBg(){var off=bgOff();
  $('bg-photo').classList.toggle('bg-off',off);$('bg-scrim').classList.toggle('bg-off',off);
  $('btn-bg').title=off?CJ.bgshow:CJ.bghide;$('btn-bg').setAttribute('aria-label',$('btn-bg').title);
  $('btn-bg').setAttribute('aria-pressed',off?'true':'false')}
$('btn-bg').addEventListener('click',function(){
  try{localStorage.setItem('hotify-bg-off',bgOff()?'0':'1')}catch(e){}paintBg()});
paintBg();
/* ── 轮询+增量刷新（?json=1；数值变色闪/日志 prepend/TopIP keyed 原地更新）── */
var paused=false;
if(REFRESH_URL){paused=REFRESH===0}else{try{paused=localStorage.getItem('cf2-console-paused')==='1'}catch(e){}}
var lastHead=null,lastIPs='';
var log0=$('log').firstChild;if(log0)lastHead=log0.textContent;
function humanCount(n){return String(n).replace(/\B(?=(\d{3})+(?!\d))/g,',')}
function humanDur(sec){ /* humanDur Go 版同构：取全档拼接，末位 0 省略 */
  sec=Math.floor(sec);var d=Math.floor(sec/86400),h=Math.floor(sec%86400/3600),m=Math.floor(sec%3600/60),s=sec%60,p=[];
  if(d)p.push(CJ.zh?d+' 天':d+'d');if(h)p.push(CJ.zh?h+' 小时':h+'h');
  if(m)p.push(CJ.zh?m+' 分钟':m+'m');if(s)p.push(CJ.zh?s+' 秒':s+'s');
  return p.length?p.join(' '):(CJ.zh?'0 秒':'0s')}
var GLY=[['✓','OK'],['✗','ERR'],['⚠','WARN'],['🧨','BAN']];
function asci(s){for(var i=0;i<GLY.length;i++)s=s.split(GLY[i][0]).join(GLY[i][1]);return s}
function setVal(id,v,noflash){var el=$(id);v=String(v);
  if(el&&el.textContent!==v){el.textContent=v;
    if(!noflash){el.classList.remove('flash');void el.offsetWidth;el.classList.add('flash')}}}
function setCell(code,n){n=n||0;setVal('sc-'+code+'-n',humanCount(n));$('sc-'+code).classList.toggle('dim',!n)}
/* ── 运行时长本地计时（2026-09-01 用户裁定：轮询间隔内数字不动/暂停即冻结=ux 差）──
   锚点=SSR data-sec（或最近 poll 的 uptime_sec）+取数时刻；每秒本地推算走表；poll 顺带重锚
   （零额外请求，治传输时延漂移）；暂停数据轮询不影响走表——时长是墙钟不是数据。 */
var upSec=parseFloat($('v-uptime').getAttribute('data-sec')||'0')||0,upAt=Date.now();
function tickUptime(){setVal('v-uptime',CJ.uptime+' '+humanDur(upSec+(Date.now()-upAt)/1000),true)}
tickUptime();
setInterval(tickUptime,1000);
/* TopIP keyed 更新：行以 ip 为键原地改数/挪序/增删，禁整表重建——
   重建会杀掉行上的光效 lit 态(2026-08-30 实测:demo 每 5s 重建=光斑反复消失误判"错乱") */
function syncIPs(ips){
  var tb=$('ipbody'),structChanged=false,i,tr;
  var rows={};
  for(i=tb.children.length-1;i>=0;i--){var ch=tb.children[i];
    if(ch.dataset&&ch.dataset.ip)rows[ch.dataset.ip]=ch;
    else{tb.removeChild(ch);structChanged=true} /* 清空态占位行 */ }
  if(!ips.length){
    var tr0=document.createElement('tr'),td0=document.createElement('td');
    td0.colSpan=2;td0.className='text-secondary';td0.textContent=CJ.empty;
    tr0.appendChild(td0);tb.appendChild(tr0);structChanged=true;
  }
  for(i=0;i<ips.length;i++){
    var row=ips[i];tr=rows[row.ip];
    if(!tr){tr=document.createElement('tr');tr.dataset.ip=row.ip;
      var a=document.createElement('td');a.textContent=row.ip;
      var b=document.createElement('td');b.className='text-end';
      tr.appendChild(a);tr.appendChild(b);structChanged=true}
    var cell=tr.children[1],txt=humanCount(row.count);
    if(cell.textContent!==txt)cell.textContent=txt; /* 纯数值原地写,不动节点 */
    if(tb.children[i]!==tr){tb.insertBefore(tr,tb.children[i]||null);structChanged=true}
    delete rows[row.ip];
  }
  for(var ip in rows){tb.removeChild(rows[ip]);structChanged=true} /* 掉出 Top10 的 */
  if(structChanged&&window.__lightRepaint)window.__lightRepaint();
}
function poll(){
  fetch('?json=1').then(function(r){return r.json()}).then(function(d){
    setVal('v-total',humanCount(d.issued));setVal('v-today','+'+humanCount(d.issued_today));
    if(d.uptime_sec){upSec=d.uptime_sec;upAt=Date.now()} /* 运行时长重锚，本地计时续走 */
    var st=d.status||{};
    setCell(401,st['401']);setCell(403,st['403']);setCell(429,st['429']);setCell(500,st['500']);
    setVal('sc-ban-n',humanCount(d.bans||0));$('sc-ban').classList.toggle('dim',!d.bans);
    var ips=d.top_ips||[],key=JSON.stringify(ips);
    if(key!==lastIPs){lastIPs=key;syncIPs(ips)}
    var rec=(d.recent||[]).map(asci);
    if(rec.length){var box=$('log'),ni=0;
      while(ni<rec.length&&rec[ni]!==lastHead)ni++;
      for(var i=ni-1;i>=0;i--){var div=document.createElement('div');
        div.textContent=rec[i];div.className='row-in';box.insertBefore(div,box.firstChild)}
      lastHead=rec[0];while(box.children.length>200)box.removeChild(box.lastChild)}
  }).catch(function(){}) /* 失败静默：下轮自愈，不伪装在线也不刷屏 */
}
function paintPause(){$('ic-pause').classList.toggle('is-off',paused);
  $('ic-play').classList.toggle('is-off',!paused);
  $('btn-pause').title=paused?CJ.autorefresh:CJ.pause;
  $('btn-pause').setAttribute('aria-label',$('btn-pause').title)} /* 刷新图标静态(2026-08-30 用户裁定) */
$('btn-pause').addEventListener('click',function(){paused=!paused;
  try{localStorage.setItem('cf2-console-paused',paused?'1':'0')}catch(e){}
  paintPause();if(!paused)poll()});
$('btn-ref').addEventListener('click',function(){poll()});
paintPause();
setInterval(function(){if(!paused)poll()},(REFRESH>0?REFRESH:5)*1000);
document.addEventListener('visibilitychange',function(){if(!document.hidden&&!paused)poll()}); /* 回前台补拉 */
})();
</script>
<script>
/* ── 光感面板（server 10-theme LP_FIELDS 注册表同构 2026-08-30 对齐；分主题记忆；
     尾序=存档→应用→回显→重画 canvas——静默改动无输入事件=旧帧残留）── */
(function(){
"use strict";
var $=function(i){return document.getElementById(i)};
/* 字段单一注册表：一行=一字段 key/cssVar/def/el+out+parse+fmt(px=r 单位后缀)/global(blob 全局档)。
   加滑杆=加一行（server TD-webui-4 清账同款）。CF2 本地差异：无 lbl-* 动态文案（Go SSR 直出）。 */
var LP_FIELDS=[
  {key:'gain',cssVar:'--light-gain',def:1,el:'lp-gain',out:'lp-gain-v',parse:parseFloat,fmt:function(v){return Math.round(v*100)+'%'}},
  {key:'edge',cssVar:'--light-edge-base',def:.20,el:'lp-edge',out:'lp-edge-v',parse:parseFloat,fmt:function(v){return Math.round(v*100)+'%'}},
  {key:'r',cssVar:'--light-r',def:80,el:'lp-r',out:'lp-r-v',parse:parseInt,fmt:function(v){return v+'px'},px:true},
  {key:'ds',cssVar:'--light-dsurface',def:.03,el:'lp-ds',out:'lp-ds-v',parse:parseFloat,fmt:function(v){return '+'+Math.round(v*100)+'%'}},
  {key:'rgb',cssVar:'--light-rgb',def:''}, /* 三入口专道(swatch/picker/hex)，空=品牌色回落 */
  {key:'blob',cssVar:'--light-blob',def:true,global:true} /* 光球开关：全局档，关=写 '0'(只关光球) */
];
var LP_DEF={};LP_FIELDS.forEach(function(f){LP_DEF[f.key]=f.def});
var LP_SWATCHES=[['6,111,209','#066fd1'],['16,152,173','#1098ad'],['0,169,165','#00a9a5'],['47,179,128','#2fb380'],
  ['240,160,32','#f0a20e'],['232,89,12','#e8590c'],['214,51,108','#d6336c'],['112,72,232','#7048e8']];
var lpPref=(function(){try{var v=JSON.parse(localStorage.getItem('hotify-light-pref'));
  return (v&&typeof v==='object'&&!Array.isArray(v))?v:{}}catch(e){return{}}})(); /* 类型闸:合法 JSON 非对象炸面板 */
function lpTheme(){return document.documentElement.getAttribute('data-bs-theme')==='dark'?'dark':'light'}
function lpVals(){var p=lpPref[lpTheme()]||{},out={};for(var k in LP_DEF)out[k]=p[k]!=null?p[k]:LP_DEF[k];return out}
function lpApply(){ /* 只写偏离默认的变量：回落 :root/档定义（恢复默认=删光该主题档） */
  var v=lpVals(),h=document.documentElement.style;
  LP_FIELDS.forEach(function(f){
    var val=f.global?(lpPref[f.key]!=null?lpPref[f.key]:f.def):v[f.key];
    var off=val===f.def;
    if(f.key==='blob'){off?h.removeProperty(f.cssVar):h.setProperty(f.cssVar,'0');return}
    if(off){h.removeProperty(f.cssVar);return}
    h.setProperty(f.cssVar,f.px?val+'px':String(val));
  });
}
function lpSave(){try{localStorage.setItem('hotify-light-pref',JSON.stringify(lpPref))}catch(e){}}
function lpCommit(){lpSave();lpApply();lpSync(); /* 尾序单一 owner */
  if(window.__lightRepaint)window.__lightRepaint()}
function lpSet(key,val){ /* 写档（等于默认值时删键——档里只存偏离） */
  var f=LP_FIELDS.filter(function(x){return x.key===key})[0];
  if(f.global){lpPref[key]=val;if(val===f.def)delete lpPref[key]}
  else{var p=lpPref[lpTheme()]=lpPref[lpTheme()]||{};
    if(val===f.def)delete p[key];else p[key]=val;
    if(!Object.keys(p).length)delete lpPref[lpTheme()]}
  lpCommit()}
function lpHexToRgb(s){var m=/^#?([0-9a-f]{6})$/i.exec(s&&s.trim());if(!m)return null;
  var n=parseInt(m[1],16);return ((n>>16)&255)+','+((n>>8)&255)+','+(n&255)}
function lpSync(){ /* 控件←当前主题档；左下灰字=当前主题名（档位指示） */
  var v=lpVals();
  $('lp-hint').textContent=lpTheme()==='dark'?CJ.themedark:CJ.themelight;
  LP_FIELDS.forEach(function(f){if(f.el){$(f.el).value=v[f.key];$(f.out).textContent=f.fmt(v[f.key])}});
  var hex=v.rgb?'#'+v.rgb.split(',').map(function(x){return (+x).toString(16).padStart(2,'0')}).join(''):'';
  $('lp-hex').value=hex.toUpperCase();if(hex)$('lp-picker').value=hex;
  document.querySelectorAll('.lp-sw').forEach(function(b){
    b.classList.toggle('on',b.getAttribute('data-rgb')===v.rgb)});
  $('lp-blob').checked=lpPref.blob!==false}
LP_SWATCHES.forEach(function(sw){var b=document.createElement('button');b.type='button';b.className='lp-sw';
  b.setAttribute('data-rgb',sw[0]);b.title=sw[1];b.style.background=sw[1];
  b.addEventListener('click',function(){lpSet('rgb',sw[0])});$('lp-swatches').appendChild(b)});
LP_FIELDS.forEach(function(f){if(f.el)$(f.el).addEventListener('input',function(){lpSet(f.key,f.parse(this.value))})});
$('lp-picker').addEventListener('input',function(){var rgb=lpHexToRgb(this.value);if(rgb)lpSet('rgb',rgb)});
$('lp-hex').addEventListener('change',function(){var rgb=lpHexToRgb(this.value);if(rgb)lpSet('rgb',rgb);else lpSync()});
$('lp-blob').addEventListener('change',function(){lpSet('blob',this.checked)});
/* anchorPanel（server 00-core 同款移植）：fixed 面板锚钮正下方水平居中（读 rect 动态定位），
   视口 clamp 两侧 ≥8px；scroll/resize rAF 合帧自动重锚；窄屏清 inline 让位 bottom sheet */
var anchored=new WeakMap();
function anchorPanel(panel,anchorEl){
  if(!panel||!anchorEl)return function(){};
  var place=function(){
    if(window.matchMedia('(max-width: 767.98px)').matches){panel.style.top='';panel.style.left='';panel.style.right='';return}
    var b=anchorEl.getBoundingClientRect();
    panel.style.top=Math.max(Math.round(b.bottom+8),8)+'px';
    var w=panel.offsetWidth||312,cx=b.left+b.width/2;
    panel.style.left=Math.round(Math.min(Math.max(cx-w/2,8),window.innerWidth-w-8))+'px';
    panel.style.right='';
  };
  place();
  if(!anchored.has(panel)){
    var queued=false;
    var reflow=function(){if(queued)return;queued=true;requestAnimationFrame(function(){queued=false;place()})};
    window.addEventListener('scroll',reflow,{passive:true});
    window.addEventListener('resize',reflow,{passive:true});
    anchored.set(panel,function(){
      window.removeEventListener('scroll',reflow);
      window.removeEventListener('resize',reflow);
      anchored.delete(panel);
    });
  }
  return anchored.get(panel);
}
function lpToggle(){var p=$('light-panel');
  if(p.classList.contains('is-off')){lpSync();anchorPanel(p,$('btn-palette'));
    p.classList.remove('is-off');p.classList.remove('view-in');void p.offsetWidth;p.classList.add('view-in')}
  else p.classList.add('is-off')}
$('btn-palette').addEventListener('click',function(e){e.stopPropagation();lpToggle()});
$('lp-close').addEventListener('click',function(){$('light-panel').classList.add('is-off')});
document.addEventListener('click',function(e){var p=$('light-panel');
  if(p.classList.contains('is-off'))return;
  if(p.contains(e.target)||e.target.closest('#btn-palette'))return;
  p.classList.add('is-off')});
window.CF2ThemeApplied=lpApply; /* 分主题档重应用：主题切换器在 attr 真翻转后回调（VT 时序正确；
  server 版 setTimeout(0) 第二监听在 VT 下先于 apply 跑=读旧主题） */
$('lp-reset').addEventListener('click',function(){delete lpPref[lpTheme()];delete lpPref.blob;lpCommit()});
lpApply(); /* 启动即应用记忆档 */
})();
</script>
<script>
/* ── 沉浸光感·全局 Canvas 渲染器（server 15-light.js 同构 2026-08-30 对齐，CSS 四通道版退役）：
     四通道（①光球 sprite ②面斑 clip ③边缘带段化 ④input 环+斑）单一绘制循环画进 #light-canvas。
     CF2 本地差异三处：SEL/SEL_ROW=本页受光名单；无 modal=无光域切换（canvas 恒页面域 z1049，
     #light-panel(1060) 天然挡光=server 2026-09-01 光域 CP 同语义）；密集表行加 --light-row-r 小半径档
     （server 无 45px 密集行；大半径多行同亮=「位置错乱」，2026-08-30 用户报）。── */
(function(){
"use strict";
var cv=document.getElementById('light-canvas');
var ctx=cv&&cv.getContext?cv.getContext('2d'):null; /* desynchronized:true 不用(server 真机黑屏回滚) */
var SEL='.card, .capsule, #ipbody tr'; /* 受光面注册表（名单单一真相；行必须进 SEL 本体——server 同构
  .list-group-item 也在 SEL，SEL_ROW 是其分隔线形态子集标记，不进 SEL 的行全扫收不到） */
var SEL_ROW='#ipbody tr'; /* 行分隔线：边缘带只画顶边（行另有面斑余晖） */
var SEL_LINE_TOP='.hr'; /* 纯分隔线家族-顶线（2026-09-01 用户报 .hr/card-header 无光=线家族注册缺口，
  非第二机制：无面斑只画线带；.hr 是 border-top div 零高，阈值豁免见 collect） */
var SEL_LINE_BOT='.card-header, .card-table thead th'; /* 纯分隔线家族-底线：卡头下缘/表头下缘 */
var INPUTS='.form-control, .form-select';
var SEL_ALL=SEL+','+SEL_LINE_TOP+','+SEL_LINE_BOT+','+INPUTS; /* 受光面全集单一真相（全扫与触屏手指栈共用一份） */
var px=-1,py=-1,pending=null,pendCand=null,lit=[];

function vars(){
  var cs=getComputedStyle(document.documentElement);
  var gain=parseFloat(cs.getPropertyValue('--light-gain'));
  if(isNaN(gain)||gain<0)gain=1; /* 0=滑杆最左「关灯」合法档（!gain 曾把 0 静默回 1=死档） */
  var rgb=cs.getPropertyValue('--light-rgb').trim();
  /* 读端兜底（server 同款）：手改 localStorage 可 inline 毒值逐帧炸绘制；非严格 rgb 串回落 */
  if(!/^\d{1,3},\d{1,3},\d{1,3}$/.test(rgb))rgb=cs.getPropertyValue('--tblr-primary-rgb').trim()||'6, 111, 209';
  var r=parseFloat(cs.getPropertyValue('--light-r'));
  if(!(r>=10)||r>400)r=80;
  var eb=parseFloat(cs.getPropertyValue('--light-edge-base'));
  if(!(eb>=0)||eb>1||!isFinite(eb))eb=.20;
  var rr=parseFloat(cs.getPropertyValue('--light-row-r'));
  if(!(rr>=10)||rr>400)rr=36; /* CF2 密集行档 */
  return {
    r:r,rowR:rr,
    blob:cs.getPropertyValue('--light-blob').trim()!=='0', /* 光球开关：只关通道①，面斑/边缘带照常 */
    aBase:parseFloat(cs.getPropertyValue('--light-a-base'))||.13,
    gain:gain,
    edgeA:eb*gain, /* 边缘/input 环独立档 */
    ds:parseFloat(cs.getPropertyValue('--light-dsurface')), /* isNaN=未设走 0 */
    falloff:(parseFloat(cs.getPropertyValue('--light-falloff'))||65)/100,
    rgb:rgb
  };
}
/* canvas 尺寸/缩放：物理像素×渲染档（coarse 减半）；视口高锁 --vp-h 同壁纸（键盘期不缩不清屏） */
var SC=1;
function sizeCanvas(){
  if(!ctx)return;
  var dpr=window.devicePixelRatio||1;
  var rs=(window.matchMedia&&matchMedia('(pointer: coarse)').matches)?.5:1;
  SC=dpr*rs;
  cv.style.width='100vw';cv.style.height='var(--vp-h, 100vh)';
  cv.width=Math.round(cv.clientWidth*SC);
  cv.height=Math.round(cv.clientHeight*SC);
}
if(ctx){window.addEventListener('resize',function(){sizeCanvas();if(px>=0)queue()});sizeCanvas()}
/* 光球 sprite：预渲染 128px 径向位图走 drawImage 快路径；键=rgb|α|falloff，调参才重烤 */
var blobSp=null,blobKey='';
function blobSprite(v,blobA){
  var key=v.rgb+'|'+blobA.toFixed(3)+'|'+v.falloff;
  if(blobKey!==key){
    blobKey=key;
    var S=128;
    blobSp=document.createElement('canvas');
    blobSp.width=S;blobSp.height=S;
    var c2=blobSp.getContext('2d');
    var g=c2.createRadialGradient(S/2,S/2,0,S/2,S/2,S/2);
    g.addColorStop(0,rgba(v,blobA));
    g.addColorStop(Math.min(1,v.falloff),rgba(v,0));
    c2.fillStyle=g;c2.fillRect(0,0,S,S);
  }
  return blobSp;
}
/* 圆角读取（每元素一次缓存） */
var radCache=new WeakMap();
function radiusOf(el){
  var v=radCache.get(el);
  if(v===undefined){
    v=parseFloat(getComputedStyle(el).borderTopLeftRadius)||0;
    if(v>32)v=32;
    radCache.set(el,v);
  }
  return v;
}
/* ── 通用绘制原语 ── */
function dist(x1,y1,x2,y2){var dx=x1-x2,dy=y1-y2;return Math.sqrt(dx*dx+dy*dy)}
function rgba(v,a){return 'rgba('+v.rgb+','+Math.max(0,Math.min(1,a)).toFixed(3)+')'}
function radial(v,x,y,radius,a0){ /* α0 at 中心 → falloff% 半径处透明 */
  var g=ctx.createRadialGradient(x,y,0,x,y,radius);
  g.addColorStop(0,rgba(v,a0));
  g.addColorStop(Math.min(1,v.falloff),rgba(v,0));
  return g;
}
function roundRect(x,y,w,h,rad){
  ctx.beginPath();
  rad=Math.min(rad,w/2,h/2);
  ctx.moveTo(x+rad,y);
  ctx.arcTo(x+w,y,x+w,y+h,rad);
  ctx.arcTo(x+w,y+h,x,y+h,rad);
  ctx.arcTo(x,y+h,x,y,rad);
  ctx.arcTo(x,y,x+w,y,rad);
  ctx.closePath();
}
/* ── 边缘带周长段绘制（段级 α+段内微渐变+阈值）：span 旧 CSS 视觉锁 r×1.4×70% ── */
var SEG=4,SEG_MIN_A=.06;
function edgeAlpha(v,x,y,mx,my,k,span){
  var d=dist(x,y,mx,my);
  return v.edgeA*k*Math.max(0,1-d/span);
}
function segLine(x1,y1,x2,y2,v,mx,my,k,span){
  var a1=edgeAlpha(v,x1,y1,mx,my,k,span),a2=edgeAlpha(v,x2,y2,mx,my,k,span);
  if(a1<SEG_MIN_A&&a2<SEG_MIN_A)return;
  /* 纯色快路径：两端 α 差<0.02 的段（远离指针的均匀区=大多数）跳过 createLinearGradient */
  if(Math.abs(a1-a2)<.02){ctx.strokeStyle=rgba(v,(a1+a2)/2)}
  else{
    var g=ctx.createLinearGradient(x1,y1,x2,y2);
    g.addColorStop(0,rgba(v,a1));
    g.addColorStop(1,rgba(v,a2));
    ctx.strokeStyle=g;
  }
  ctx.beginPath();
  ctx.moveTo(x1,y1);ctx.lineTo(x2,y2);
  ctx.stroke();
}
function drawEdgeBand(v,o,mx,my){ /* o={el,b,k,row} row:'top'|'bot'|undefined=整周 */
  var b=o.b,k=o.k,row=o.row;
  var rad=radiusOf(o.el);
  /* 内缩 1px：stroke 线中心内外各半；线模式以交界线居中——top 路径 y=行顶（border-collapse 交界线
    正中=top，lineWidth2 带落 [top-1,top+1] 恰跨线两翼）。曾 y=top-1（server 逐字继承）：带落 [top-2,top]
    整体悬上 1px（server 注释自述"-1~+1 居中"但代码实为 -2~0=注释差 1px；2026-09-01 用户报行交界偏上）。
    bot=元素底缘 y=bottom-1（border-bottom 占 [bottom-1,bottom]，带中心差 0.5px 亚视觉不动） */
  var x=b.left+1,w=b.width-2,h=b.height-2;
  var y=row==='top'?b.top:(row==='bot'?b.bottom-1:b.top+1);
  if(w<=0||(!row&&h<=0))return; /* 线模式零高合法（.hr=border-top div，h 为负只看 w） */
  var rr=Math.min(rad,w/2,h/2);
  var span=(row?v.rowR:v.r)*1.4*.7; /* CF2：线族全走小半径档 */
  ctx.lineWidth=2;ctx.lineCap='butt';
  function walk(pts){
    for(var i=0;i+1<pts.length;i++){
      var p1=pts[i],p2=pts[i+1],d=dist(p1[0],p1[1],p2[0],p2[1]);
      if(d<=SEG){segLine(p1[0],p1[1],p2[0],p2[1],v,mx,my,k,span);continue}
      for(var t=0;t<d;t+=SEG){
        var e=Math.min(SEG,d-t),f1=t/d,f2=(t+e)/d;
        segLine(p1[0]+(p2[0]-p1[0])*f1,p1[1]+(p2[1]-p1[1])*f1,
                p1[0]+(p2[0]-p1[0])*f2,p1[1]+(p2[1]-p1[1])*f2,v,mx,my,k,span);
      }
    }
  }
  function arcPts(cx,cy,a0,a1){ /* 圆角弧采样（2px 弧长） */
    var n=Math.max(2,Math.ceil(Math.abs(a1-a0)*rr/2)),out=[];
    for(var i=0;i<=n;i++){var a=a0+(a1-a0)*i/n;out.push([cx+rr*Math.cos(a),cy+rr*Math.sin(a)])}
    return out;
  }
  var pts=row?[[x,y],[x+w,y]]:[[x+rr,y]]; /* row=分隔线只画顶边全宽 */
  if(!row)pts=pts.concat(arcPts(x+w-rr,y+rr,-Math.PI/2,0));
  if(!row)pts=pts.concat([[x+w,y+h-rr]]).concat(arcPts(x+w-rr,y+h-rr,0,Math.PI/2));
  if(!row)pts=pts.concat([[x+rr,y+h]]).concat(arcPts(x+rr,y+h-rr,Math.PI/2,Math.PI));
  if(!row)pts=pts.concat([[x,y+rr]]).concat(arcPts(x+rr,y+rr,Math.PI,Math.PI*1.5));
  walk(pts);
}
/* 收集：谓词管道（面积阈值→视口外剔除在 rect 读后）+分桶合一，paint 纯绘制 */
function collect(candidates,v){
  var list=candidates||document.querySelectorAll(SEL_ALL);
  var reads=[];
  list.forEach?list.forEach(function(el){
    var line=el.matches(SEL_LINE_TOP)||el.matches(SEL_LINE_BOT); /* 结构谓词在 rect 读前（便宜先行） */
    var b=el.getBoundingClientRect();
    if(b.width<4)return;
    if(!line&&b.height<4)return; /* 线家族零高合法（.hr）；面家族亚视觉面兜底（display:none 幽灵等） */
    if(b.bottom<-v.r||b.top>window.innerHeight+v.r||b.right<-v.r||b.left>window.innerWidth+v.r)return; /* 视口外不进绘制 */
    reads.push({el:el,b:b,line:line});
  }):null;
  var faces=[],inputs=[],next=[],onSurface=false;
  reads.forEach(function(o){
    var b=o.b,isRow=o.el.matches(SEL_ROW);
    var lineT=o.el.matches(SEL_LINE_TOP),lineB=o.el.matches(SEL_LINE_BOT);
    var isLine=lineT||lineB;
    var rr=(isRow||isLine)?v.rowR:v.r; /* 线族全走小半径档（行/纯线同律） */
    var dx=Math.max(b.left-px,0,px-b.right);
    var dy=Math.max(b.top-py,0,py-b.bottom);
    var k=1-Math.sqrt(dx*dx+dy*dy)/(rr*2); /* 计算器核心式 */
    if(k<=0)return;
    var mx=(px-b.left),my=(py-b.top); /* 指针在元素坐标系 */
    if(k>=1&&o.el.matches('.card'))onSurface=true; /* 卡面补偿判据（server 同款） */
    if(o.el.matches(INPUTS)){
      if(document.activeElement===o.el)return; /* focus 让位：原生 focus 环是标准交互指示 */
      inputs.push({el:o.el,b:b,k:k,mx:mx,my:my});
    }else{
      o.el.classList.add('lit');
      if(isRow)o.el.classList.add('lit-row');
      faces.push({el:o.el,b:b,k:k,mx:mx,my:my,
        row:isRow||lineT?'top':(lineB?'bot':false),lineOnly:isLine}); /* 'top'/'bot'=线,false=整周 */
      next.push(o.el);
    }
  });
  return {faces:faces,inputs:inputs,onSurface:onSurface,next:next};
}
function paint(candidates){
  pending=null;
  if(px<0){unlit();return}
  var v=vars();
  var c=collect(candidates,v),faces=c.faces,inputs=c.inputs,next=c.next,onSurface=c.onSurface;
  ctx.clearRect(0,0,cv.width,cv.height);
  /* ① 光球：α=base×gain(+on-surface 卡面补偿Δ) */
  var blobA=(v.aBase+(onSurface?(isNaN(v.ds)?0:v.ds):0))*v.gain;
  if(window.__lightBoost)blobA=.45;
  if(v.blob)ctx.drawImage(blobSprite(v,blobA),(px-v.r)*SC,(py-v.r)*SC,v.r*2*SC,v.r*2*SC);
  /* ② 面斑+③ 边缘带 */
  faces.forEach(function(o){
    var b=o.b,rad=radiusOf(o.el);
    if(!o.lineOnly){ /* 纯分隔线无面斑只画线带（卡头区域已有整卡面斑，再叠=双亮） */
      ctx.save();
      roundRect(b.left*SC,b.top*SC,b.width*SC,b.height*SC,rad*SC);
      ctx.clip();
      ctx.fillStyle=radial(v,(b.left+o.mx)*SC,(b.top+o.my)*SC,v.r*SC,v.aBase*v.gain*o.k);
      ctx.fillRect(b.left*SC,b.top*SC,b.width*SC,b.height*SC);
      ctx.restore();
    }
    ctx.save();
    ctx.scale(SC,SC);
    drawEdgeBand(v,o,b.left+o.mx,b.top+o.my);
    ctx.restore();
  });
  /* ④ input：框内斑（edgeA+.08×gain）+环（段化圆角） */
  inputs.forEach(function(o){
    var b=o.b,rad=radiusOf(o.el);
    ctx.save();
    roundRect(b.left*SC,b.top*SC,b.width*SC,b.height*SC,rad*SC);
    ctx.clip();
    ctx.fillStyle=radial(v,(b.left+o.mx)*SC,(b.top+o.my)*SC,v.r*SC,(v.edgeA+.08*v.gain)*o.k);
    ctx.fillRect(b.left*SC,b.top*SC,b.width*SC,b.height*SC);
    ctx.restore();
    ctx.save();
    ctx.scale(SC,SC);
    drawEdgeBand(v,o,b.left+o.mx,b.top+o.my);
    ctx.restore();
  });
  clearLit(next);
}
function clearLit(keep){ /* lit 类清理单点（全清与差集清两处同族操作合一） */
  lit.forEach(function(el){if(keep.indexOf(el)<0){el.classList.remove('lit');el.classList.remove('lit-row')}});
  lit=keep;
}
function unlit(){
  if(ctx)ctx.clearRect(0,0,cv.width,cv.height);
  clearLit([]);
}
function queue(candidates){
  if(candidates)pendCand=candidates;
  if(!pending)pending=requestAnimationFrame(function(){pending=null;var c=pendCand;pendCand=null;paint(c)});
}
if(window.matchMedia&&matchMedia('(prefers-reduced-motion: reduce)').matches)return; /* 红线：跳过受光 */
var touchActive=false;
document.addEventListener('pointermove',function(ev){px=ev.clientX;py=ev.clientY;queue()},{passive:true});
/* 滚动跟手：scroll 只当「正在滚」信号置活跃戳，帧循环活跃期每帧重画（rect 每帧新读=与合成器
   零相位差），停滚 120ms 自动静默；capture=scroll 不冒泡，内滚容器也要触发 */
var scrollAlive=0;
window.addEventListener('scroll',function(){scrollAlive=performance.now()+120;queue()},{passive:true,capture:true});
(function scrollFollow(){
  if(performance.now()<scrollAlive&&px>=0)queue();
  requestAnimationFrame(scrollFollow);
})();
document.addEventListener('pointerleave',function(){if(touchActive)return;px=-1;py=-1;queue()});
/* 触屏拖拽光源：候选=手指栈缓存（touchstart 算+位移>40px 才重算） */
function touchAt(ev){
  if(ev.touches&&ev.touches[0]){
    px=ev.touches[0].clientX;py=ev.touches[0].clientY;
    if(document.elementsFromPoint){
      var moved=Math.abs(px-lastStackX)>40||Math.abs(py-lastStackY)>40;
      if(ev.type==='touchstart'||!touchStack||moved){
        lastStackX=px;lastStackY=py;
        touchStack=document.elementsFromPoint(px,py).filter(function(el){
          return el.nodeType===1&&el.matches&&el.matches(SEL_ALL);
        });
      }
      queue(touchStack.length?touchStack:null);
      return;
    }
    queue();
  }
}
var lastStackX=-99,lastStackY=-99,touchStack=null;
document.addEventListener('touchstart',function(ev){touchActive=true;touchAt(ev)},{passive:true});
document.addEventListener('touchmove',touchAt,{passive:true});
document.addEventListener('touchend',function(ev){touchActive=false;if(!ev.touches.length){px=-1;py=-1;queue()}},{passive:true});
/* ?light= 自证通道（server 同款）：boost=α .45+normal 混合；noblend=全链 normal 消融；bench=帧率 HUD */
var lightMode=(/[?&]light=([a-z-]+)/.exec(location.search)||[])[1]||'';
window.__lightRepaint=function(){touchStack=null;queue()}; /* 外部重画钩：面板静默改动后重画+TopIP 结构变更 */
if(lightMode==='boost'){window.__lightBoost=true;if(ctx)cv.style.mixBlendMode='normal'}
if(lightMode.indexOf('noblend')>=0)document.documentElement.classList.add('noblend');
var lastBench=performance.now();
if(lightMode.indexOf('bench')===0)(function bench(){
  var hud=document.createElement('div');
  hud.style.cssText='position:fixed;left:6px;top:6px;z-index:3000;pointer-events:none;font:11px/1.5 monospace;'+
    'background:rgba(0,0,0,.75);color:#8fd;padding:6px 8px;border-radius:6px;white-space:pre';
  document.body.appendChild(hud);
  var all=[],drag=[],collecting=false;
  function stats(a){if(a.length<5)return '—';
    var s=a.slice().sort(function(x,y){return x-y});
    var fps=Math.round(1000/(s.reduce(function(p,c){return p+c},0)/s.length));
    return fps+'fps p95='+s[Math.floor(s.length*.95)].toFixed(1)+'ms 掉帧='+s.filter(function(x){return x>22}).length}
  document.addEventListener('touchstart',function(){drag=[];collecting=true},{passive:true});
  document.addEventListener('touchend',function(){setTimeout(function(){collecting=false},400)},{passive:true});
  (function loop(now){
    var dt=now-lastBench;lastBench=now;
    if(dt>3&&dt<300){all.push(dt);if(collecting)drag.push(dt)}
    if(all.length>2000)all=all.slice(-1000);
    hud.textContent='bench'+(lightMode.indexOf('noblend')>=0?'(noblend)':'')+
      '\n全程: '+stats(all)+'\n最近拖动: '+stats(drag);
    requestAnimationFrame(loop);
  })(performance.now());
})();
})();
</script>
</body></html>`
}
