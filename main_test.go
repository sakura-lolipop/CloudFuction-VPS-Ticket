package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// 测试用 SA：运行时生成 RSA 密钥（非真凭证；只验结构/流程，不打华为）。

func genTestSA(t *testing.T) {
	t.Helper()
	priv := genRSA(t)
	os.Setenv("PRIVATE_JSON", saJSONWithKey(t, priv))
	t.Cleanup(func() { os.Unsetenv("PRIVATE_JSON") })
}

func doTicket(r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handleTicket(rec, r)
	return rec
}

func TestTicketContractAnonymous(t *testing.T) {
	resetGuard()
	genTestSA(t)
	os.Unsetenv("TICKET_AUTH_TOKEN") // 匿名（当前默认）；限速/auto-ban 全默认关=直通

	rec := doTicket(httptest.NewRequest("POST", "/ticket", nil))
	if rec.Code != 200 {
		t.Fatalf("匿名直通 = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertTicketContract(t, rec.Body.Bytes(), 600) // 默认 TTL=600
}

func TestAuth(t *testing.T) {
	resetGuard()
	genTestSA(t)
	os.Setenv("TICKET_AUTH_TOKEN", "sec123")
	t.Cleanup(func() { os.Unsetenv("TICKET_AUTH_TOKEN") })

	req := httptest.NewRequest("POST", "/ticket", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	if rec := doTicket(req); rec.Code != 401 {
		t.Fatalf("错 token = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest("POST", "/ticket", nil)
	req.Header.Set("Authorization", "Bearer sec123")
	if rec := doTicket(req); rec.Code != 200 {
		t.Fatalf("对 token = %d, want 200", rec.Code)
	}

	// 设了 TICKET_AUTH_TOKEN 但请求没带头 → 匿名放行（IP 记账；单 token 期不强制）
	if rec := doTicket(httptest.NewRequest("POST", "/ticket", nil)); rec.Code != 200 {
		t.Fatalf("无头（token 设了）= %d, want 200", rec.Code)
	}
}

func TestRateLimitBuckets(t *testing.T) {
	t.Cleanup(func() {
		for _, k := range []string{"TICKET_RATE_LIMIT_IP", "TICKET_RATE_LIMIT_TOKEN", "TICKET_AUTH_TOKEN", "TICKET_AUTO_BAN"} {
			os.Unsetenv(k)
		}
	})

	// IP 桶独立：IP=2 → 第 3 次 429（匿名，token 桶关）
	resetGuard()
	genTestSA(t)
	os.Setenv("TICKET_RATE_LIMIT_IP", "2")
	for i, want := range []int{200, 200, 429} {
		if rec := doTicket(httptest.NewRequest("POST", "/ticket", nil)); rec.Code != want {
			t.Fatalf("IP 桶：第 %d 次 = %d, want %d", i+1, rec.Code, want)
		}
	}

	// token 桶独立：IP 桶关、TOKEN=1 → 对 token 第 2 次 429
	resetGuard()
	os.Setenv("TICKET_AUTH_TOKEN", "sec123")
	os.Setenv("TICKET_RATE_LIMIT_TOKEN", "1")
	os.Unsetenv("TICKET_RATE_LIMIT_IP")
	authed := func() *http.Request {
		req := httptest.NewRequest("POST", "/ticket", nil)
		req.Header.Set("Authorization", "Bearer sec123")
		return req
	}
	if rec := doTicket(authed()); rec.Code != 200 {
		t.Fatalf("token 桶第 1 次 = %d, want 200", rec.Code)
	}
	if rec := doTicket(authed()); rec.Code != 429 {
		t.Fatalf("token 桶第 2 次 = %d, want 429", rec.Code)
	}
}

func TestAutoBan(t *testing.T) {
	resetGuard()
	genTestSA(t)
	os.Unsetenv("TICKET_AUTH_TOKEN")
	os.Setenv("TICKET_RATE_LIMIT_IP", "2") // 撞两次 429 → auto-ban（N=2）
	os.Setenv("TICKET_AUTO_BAN", "2")
	os.Setenv("TICKET_AUTO_BAN_SECONDS", "600")
	t.Cleanup(func() {
		for _, k := range []string{"TICKET_RATE_LIMIT_IP", "TICKET_AUTO_BAN", "TICKET_AUTO_BAN_SECONDS"} {
			os.Unsetenv(k)
		}
	})
	// 可控时钟：解封测试用（签票的 expires_at 同源，无碍）
	base := time.Now()
	cur := base
	nowFn = func() time.Time { return cur }
	t.Cleanup(func() { nowFn = time.Now })

	for i, want := range []int{200, 200, 429, 429} { // 2 张 + 第 3 次 strike#1 + 第 4 次 strike#2 触发封（该次仍 429）
		if rec := doTicket(httptest.NewRequest("POST", "/ticket", nil)); rec.Code != want {
			t.Fatalf("auto-ban 前第 %d 次 = %d, want %d", i+1, rec.Code, want)
		}
	}
	// 第 5 次：已被临时封 → 403（固定刑期，不进桶不记账）+ Retry-After 头
	rec5 := doTicket(httptest.NewRequest("POST", "/ticket", nil))
	if rec5.Code != 403 {
		t.Fatalf("封禁中 = %d, want 403; body=%s", rec5.Code, rec5.Body.String())
	}
	if rec5.Header().Get("Retry-After") == "" {
		t.Fatalf("403 缺 Retry-After 头")
	}
	// 快进 601s → 解封白纸（旧 strikes 出窗；限速桶分钟窗也过了）→ 200
	cur = base.Add(601 * time.Second)
	if rec := doTicket(httptest.NewRequest("POST", "/ticket", nil)); rec.Code != 200 {
		t.Fatalf("解封后 = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSignJWTClaims(t *testing.T) {
	genTestSA(t)
	a, err := loadSA()
	if err != nil {
		t.Fatalf("loadSA: %v", err)
	}
	jwt, err := a.SignJWT(600 * time.Second)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("非三段 JWT")
	}
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
		Aud string `json:"aud"`
	}
	if err := json.Unmarshal(b64decode(t, parts[1]), &claims); err != nil {
		t.Fatalf("payload 解码: %v", err)
	}
	if claims.Exp-claims.Iat != 600 {
		t.Fatalf("exp-iat = %d, want 600", claims.Exp-claims.Iat)
	}
	if claims.Iss != "999" || claims.Aud != tokenURI {
		t.Fatalf("iss/aud 错: %+v", claims)
	}
}

// assertTicketContract 验 200 响应三字段契约（ticket 三段 JWT / project_id / expires_at≈now+TTL）。
func assertTicketContract(t *testing.T, body []byte, ttl int64) {
	t.Helper()
	var resp struct {
		Ticket    string `json:"ticket"`
		ProjectID string `json:"project_id"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	if resp.Ticket == "" || resp.ProjectID != "1234567890" || resp.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("契约字段缺失/错值: %+v", resp)
	}
	if d := resp.ExpiresAt - time.Now().Unix() - ttl; d < -5 || d > 5 {
		t.Fatalf("expires_at TTL 偏差: %d (want ≈%d)", d, ttl)
	}
}
