// stdout 着色渲染层（移植自 HotifyNEXT-Server internal/server/color.go 2026-08-20 定版，
// cf-ticket 裁剪+列结构适配）：
//   · 骨架 `时间 | tag(补宽10) | body` 常开（管道/重定向的 stdout 也是纯文本表格）
//   · [ticket] 请求行（首 4 token = status dur ip method）再列化：
//     `时间 | [ticket] |  200  |    630us |       1.2.3.4 | GET  | ✓ ticket #2`
//     ——首 token 非 3 位数字（系统行 listen/auto-ban/…）原样走 body，天然 fallback。
//   · 颜色 TTY 才开（NOCOLOR=1 关 no-color.org / FORCE_COLOR=1 开 mintty / conhost 自动开 VT）：
//     时间戳暗灰、tag 青、状态/method 白字彩底（gin 组合码）、✓✗⚠🧊 字形色。
// 只改 stdout 的「显示」不改日志本身——logs\ticket.log 与管道收原样纯文本（ANSI 不落盘）。
package main

import (
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ANSI 色。前景 = kv 行用（zerolog 同款）；bg* = 列色块（gin logger.go 组合码：分号前=字色后=底色）。
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiBlue   = "\x1b[34m"
	ansiGray   = "\x1b[90m" // zerolog 时间戳同款暗灰
	ansiCyan   = "\x1b[36m"

	bgGreen = "\x1b[97;42m" // 白字绿底（2xx）
	bgWhite = "\x1b[90;47m" // 暗灰字白底（3xx）
	bgYellow = "\x1b[90;43m" // 暗灰字黄底（4xx）
	bgRed   = "\x1b[97;41m" // 白字红底（≥500）
	bgBlue  = "\x1b[97;44m" // 白字蓝底（GET）
	bgCyan  = "\x1b[97;46m" // 白字青底（POST）
)

func paint(color, s string) string {
	if color == "" {
		return s
	}
	return color + s + ansiReset
}

// statusBg status 底色（gin StatusCodeColor 同款分段：2xx 绿 / 3xx 白 / 4xx 黄 / 其余红）。
func statusBg(status int) string {
	switch {
	case status >= 200 && status < 300:
		return bgGreen
	case status >= 300 && status < 400:
		return bgWhite
	case status >= 400 && status < 500:
		return bgYellow
	default:
		return bgRed
	}
}

// methodBg method 底色（gin MethodColor 同款；cf-ticket 拿票 GET/POST 等价，只做视觉区分）。
func methodBg(method string) string {
	switch method {
	case "GET":
		return bgBlue
	case "POST":
		return bgCyan
	default:
		return ""
	}
}

// glyphColors body 首字形 → 色（灰蓝基调定稿：✓蓝=重点正常 / ✗红=死错 / ⚠黄=系统级拒绝 / 🧊蓝=封禁冷却）。
var glyphColors = []struct{ glyph, color string }{
	{"✓", ansiBlue},
	{"✗", ansiRed},
	{"⚠", ansiYellow},
	{"🧊", ansiBlue},
}

// stdTimestamp 匹配 stdlib log.LstdFlags 前缀（2026/08/25 11:01:12）。
var stdTimestamp = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// gridTagW tag 列宽（[ticket] 10 位补齐）。
const gridTagW = 10

func cellLeft(w int, s string) string {
	for len([]rune(s)) < w {
		s += " "
	}
	return s
}

func cellRight(w int, s string) string {
	for len([]rune(s)) < w {
		s = " " + s
	}
	return s
}

// renderLine 单行 → 骨架 `时间 | tag(补宽) | body`；[ticket] 请求行 body 走 ticketColumns
// 列化（状态/耗时/IP/method/动作），系统行原样；其余 tag 只染字形。无 tag 裸行原样（防误重排）。
func renderLine(line string, color bool) string {
	var b strings.Builder
	rest := line
	if loc := stdTimestamp.FindStringIndex(rest); loc != nil {
		tsColor := ""
		if color {
			tsColor = ansiGray
		}
		b.WriteString(paint(tsColor, rest[:loc[1]-1]))
		b.WriteString(" | ")
		rest = rest[loc[1]:]
	}
	if strings.HasPrefix(rest, "[") {
		if end := strings.Index(rest, "]"); end > 0 {
			tag, body := rest[:end+1], strings.TrimSpace(rest[end+1:])
			tagColor := ""
			if color {
				tagColor = ansiCyan // tag 青（zerolog 同款）：灰蓝基调的亮变体，不单调不抢块
			}
			b.WriteString(paint(tagColor, cellLeft(gridTagW, tag)))
			b.WriteString(" | ")
			if tag == "[ticket]" {
				b.WriteString(ticketColumns(body, color))
				return b.String()
			}
			if color {
				for _, g := range glyphColors {
					if strings.HasPrefix(body, g.glyph) {
						body = paint(g.color, g.glyph) + body[len(g.glyph):]
					}
				}
			}
			b.WriteString(body)
			return b.String()
		}
	}
	return line
}

// ticketColumns [ticket] body → 列竖线 + 色块（调用侧平铺：status dur ip method rest）：
// `  200  |    630us |       1.2.3.4 | GET  | ✓ ticket #2`。duration 显示层 normDur 规整（µ→us）；
// 首 token 非 3 位数字（系统行）原样返回（兜底，天然 fallback 不列化）。
func ticketColumns(body string, color bool) string {
	spans := tokenSpans(body)
	if len(spans) < 5 {
		return glyphify(body, color)
	}
	status, ok := parseStatus(body[spans[0][0]:spans[0][1]])
	if !ok {
		return glyphify(body, color)
	}
	dur := body[spans[1][0]:spans[1][1]]
	ip := body[spans[2][0]:spans[2][1]]
	method := body[spans[3][0]:spans[3][1]]
	rest := strings.TrimLeft(body[spans[3][1]:], " ")
	sColor, mColor := "", ""
	if color {
		sColor, mColor = statusBg(status), methodBg(method)
	}
	return paint(sColor, " "+body[spans[0][0]:spans[0][1]]+" ") + " | " +
		cellRight(10, normDur(dur)) + " | " +
		cellRight(15, ip) + " | " +
		paint(mColor, cellLeft(6, method)) + " " + glyphify(rest, color)
}

// glyphify body 首字形染色（✓✗⚠🧊）。
func glyphify(body string, color bool) string {
	if color {
		for _, g := range glyphColors {
			if strings.HasPrefix(body, g.glyph) {
				return paint(g.color, g.glyph) + body[len(g.glyph):]
			}
		}
	}
	return body
}

// normDur Duration.String() → ≤2 位小数 + us（消 µ 在中文终端的歧义宽度）：630.4µs→630us /
// 1.4818ms→1.48ms / 1.032s→1.03s / 0s→0s。解析失败原样返回（兜底）。
func normDur(d string) string {
	dur, err := time.ParseDuration(d)
	if err != nil {
		return d
	}
	switch {
	case dur == 0:
		return "0s"
	case dur < time.Millisecond:
		return strconv.FormatInt(dur.Microseconds(), 10) + "us"
	case dur < time.Second:
		return strconv.FormatFloat(dur.Seconds()*1000, 'f', 2, 64) + "ms"
	case dur < time.Minute:
		return strconv.FormatFloat(dur.Seconds(), 'f', 2, 64) + "s"
	default:
		return dur.Truncate(time.Second).String()
	}
}

// tokenSpans 空格分隔 token 的 [start,end)。
func tokenSpans(s string) [][2]int {
	var spans [][2]int
	i := 0
	for i < len(s) {
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		for i < len(s) && s[i] != ' ' {
			i++
		}
		spans = append(spans, [2]int{start, i})
	}
	return spans
}

// parseStatus 恰 3 位数字 → int。
func parseStatus(tok string) (int, bool) {
	if len(tok) != 3 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return 0, false
		}
		n = n*10 + int(tok[i]-'0')
	}
	return n, true
}

// ColorWriter io.Writer 包装：stdout 腿专用（骨架常开，颜色按 enabled）。
type ColorWriter struct {
	w       io.Writer
	enabled bool
}

func NewColorWriter(w io.Writer, enabled bool) *ColorWriter {
	return &ColorWriter{w: w, enabled: enabled}
}

// Write log 包每次写整行（含 \n），仍按行切兜底多次写。返回 len(p)（变换型 writer 惯例）。
func (cw *ColorWriter) Write(p []byte) (int, error) {
	var out []byte
	for _, line := range strings.SplitAfter(string(p), "\n") {
		if line == "" {
			continue
		}
		out = append(out, renderLine(strings.TrimSuffix(line, "\n"), cw.enabled)...)
		out = append(out, '\n')
	}
	if _, err := cw.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// DecideColor 开色判定（纯函数可测）：NOCOLOR 非空优先关；FORCE_COLOR=1 强制开（mintty）；
// 否则真 TTY（stdout 字符设备）才开。
func DecideColor(nocolor, forceColor string, stdoutIsTTY bool) bool {
	if nocolor != "" {
		return false
	}
	if forceColor == "1" {
		return true
	}
	return stdoutIsTTY
}

// stdoutIsCharDevice stdout 是否字符设备（真终端；管道/重定向 false）。
func stdoutIsCharDevice() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// ColorEnabled main 启动调：env + TTY 探测 + 平台 VT 就绪（Windows conhost 须先开 VT 才解析 ANSI）。
func ColorEnabled() bool {
	return DecideColor(os.Getenv("NOCOLOR"), os.Getenv("FORCE_COLOR"), stdoutIsCharDevice()) && vtReady()
}
