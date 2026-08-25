package main

import (
	"strings"
	"testing"
)

// 渲染层测试：锁骨架结构（列序/竖线数）与色开关语义（无色零 ANSI / 有色含 ANSI / 系统行 fallback）。

func TestRenderRequestLineNoColor(t *testing.T) {
	out := renderLine("2026/08/25 11:01:12 [ticket] 200 630.4µs 1.2.3.4 GET ✓ issue #2", false)
	want := "2026/08/25 11:01:12 | [ticket]   |  200  |      630us |         1.2.3.4 | GET    ✓ issue #2"
	if out != want {
		t.Fatalf("无色请求行:\n got  %q\n want %q", out, want)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("无色模式混入 ANSI")
	}
}

func TestRenderRequestLineColor(t *testing.T) {
	out := renderLine("2026/08/25 11:01:12 [ticket] 200 630.4µs 1.2.3.4 GET ✓ issue #2", true)
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("有色模式缺 ANSI")
	}
	// 骨架列序在色转义之外也要成立（剥 ANSI 后=无色版）
	stripped := stripANSI(out)
	want := "2026/08/25 11:01:12 | [ticket]   |  200  |      630us |         1.2.3.4 | GET    ✓ issue #2"
	if stripped != want {
		t.Fatalf("剥色后:\n got  %q\n want %q", stripped, want)
	}
}

func TestRenderSystemLineFallback(t *testing.T) {
	// 系统行（首 token 非 3 位数字）不列化，走 body
	out := renderLine("2026/08/25 11:00:00 [ticket] listen 127.0.0.1:12346 (ttl=600s proj=1016 anonymous)", false)
	want := "2026/08/25 11:00:00 | [ticket]   | listen 127.0.0.1:12346 (ttl=600s proj=1016 anonymous)"
	if out != want {
		t.Fatalf("系统行:\n got  %q\n want %q", out, want)
	}
	// auto-ban 系统行（⚠ 字形，有色时染黄）
	outColor := renderLine("2026/08/25 11:00:00 [ticket] ⚠ auto-ban ip:5.6.7.8 exp=10m0s", true)
	if !strings.Contains(outColor, "\x1b[33m⚠") {
		t.Fatalf("⚠ 字形未染黄: %q", outColor)
	}
}

func TestDecideColor(t *testing.T) {
	cases := []struct {
		nocolor, force string
		tty            bool
		want           bool
	}{
		{"", "", true, true},    // 真 TTY 开
		{"", "", false, false},  // 管道关
		{"1", "", true, false},  // NOCOLOR 优先关
		{"", "1", false, true},  // FORCE_COLOR 强开（mintty）
		{"1", "1", true, false}, // NOCOLOR 优先于 FORCE
	}
	for _, c := range cases {
		if got := DecideColor(c.nocolor, c.force, c.tty); got != c.want {
			t.Fatalf("DecideColor(%q,%q,%v)=%v want %v", c.nocolor, c.force, c.tty, got, c.want)
		}
	}
}

func stripANSI(s string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '\x1b' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++ // 吃掉 m
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
