package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// 测试用 SA（运行时生成 RSA 密钥，非真凭证；只验结构/流程，不打华为）。
const testSA = `{
  "project_id": "1234567890",
  "key_id": "testkid",
  "sub_account": "999",
  "private_key": ""
}`

// genTestSA 生成带真 RSA 私钥的 SA JSON（PEM PKCS8）。
func genTestSA(t *testing.T) string {
	t.Helper()
	priv := genRSA(t)
	saJSON := saJSONWithKey(t, priv)
	os.Setenv("PRIVATE_JSON", saJSON)
	t.Cleanup(func() { os.Unsetenv("PRIVATE_JSON") })
	return saJSON
}

func TestHandleTicketAnonymous(t *testing.T) {
	genTestSA(t)
	os.Unsetenv("TICKET_AUTH_TOKEN") // 匿名模式（当前默认，2026-08-25 裁定）

	// 无 Authorization 头 → 200 + 三字段契约
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	handleTicket(rec, req)
	if rec.Code != 200 {
		t.Fatalf("匿名 = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertTicketContract(t, rec.Body.Bytes())
}

func TestHandleTicketAuth(t *testing.T) {
	genTestSA(t)
	os.Setenv("TICKET_AUTH_TOKEN", "sec123")
	t.Cleanup(func() { os.Unsetenv("TICKET_AUTH_TOKEN") })

	// 错 token → 401
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	handleTicket(rec, req)
	if rec.Code != 401 {
		t.Fatalf("错 token = %d, want 401", rec.Code)
	}

	// 对 token → 200 + 三字段契约
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer sec123")
	rec = httptest.NewRecorder()
	handleTicket(rec, req)
	if rec.Code != 200 {
		t.Fatalf("对 token = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	assertTicketContract(t, rec.Body.Bytes())
}

// assertTicketContract 验 200 响应三字段契约（ticket 三段 JWT / project_id / expires_at≈now+TTL）。
func assertTicketContract(t *testing.T, body []byte) {
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
	if len(strings.Split(resp.Ticket, ".")) != 3 {
		t.Fatalf("ticket 非三段 JWT: %q", resp.Ticket[:min(20, len(resp.Ticket))])
	}
	if d := resp.ExpiresAt - time.Now().Unix() - 300; d < -5 || d > 5 {
		t.Fatalf("expires_at TTL 偏差: %d", d)
	}
}

func TestSignJWTShortExp(t *testing.T) {
	saJSON := genTestSA(t)
	os.Setenv("PRIVATE_JSON", saJSON)
	a, err := loadSA()
	if err != nil {
		t.Fatalf("loadSA: %v", err)
	}
	jwt, err := a.SignJWT(300 * time.Second)
	if err != nil {
		t.Fatalf("SignJWT: %v", err)
	}
	// payload 段 base64url 解码验 exp-iat == 300
	parts := strings.Split(jwt, ".")
	payload := b64decode(t, parts[1])
	var claims struct {
		Iat int64 `json:"iat"`
		Exp int64 `json:"exp"`
		Iss string `json:"iss"`
		Aud string `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("payload 解码: %v", err)
	}
	if claims.Exp-claims.Iat != 300 {
		t.Fatalf("exp-iat = %d, want 300", claims.Exp-claims.Iat)
	}
	if claims.Iss != "999" || claims.Aud != tokenURI {
		t.Fatalf("iss/aud 错: %+v", claims)
	}
}
