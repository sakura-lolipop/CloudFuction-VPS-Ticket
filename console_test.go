package main

import (
	"net/http/httptest"
	"testing"
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
