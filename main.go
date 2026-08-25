// CloudFuction-VPS-Ticket —— Hotify CF 2.0 铸票厂（VPS 形态，自包含单二进制）。
//
// 干一件事：验 token → 用华为 Service Account 签一张短 exp 的 PS256 JWT（"票"）→ 吐给调用方。
// 不碰任何通知内容（请求体可为空）——调用方（HotifyNEXT-Server）拿票自己直连华为 push-api，
// 通知内容不再过任何自有基础设施（隐私第三层）。设计依据 docs/pushkit-transport.md §9.7。
//
// 联邦：本仓是公开联邦件，任何人可 clone 部署自己的铸票节点（自己的 SA / 自己的 token）。
// 各节点签出的票带各自 project_id，票与节点天然配套，不可跨节点使用。
//
// env：
//
//	PRIVATE_JSON / PRIVATE_JSON_FILE  service account key（都不设→扫同目录含 PRIVATE KEY 的 .json）
//	TICKET_AUTH_TOKEN  铸票鉴权；不设=匿名开放（当前模式，2026-08-25 裁定；终态=云端 txt 白名单热更）
//	TICKET_TTL_SECONDS 票有效期，默认 300（以 canary 实测定案为准，见 pushkit-transport.md §9.7 待验项）
//	HOST=127.0.0.1  PORT=8091   （默认只听回环：前面有 Tunnel/nginx 反代；公网直裸再改 0.0.0.0）
//
// 响应契约（调用方 internal/pushkit 按 ticket/project_id/expires_at 三字段消费）：
//
//	GET/POST /
//	Authorization: Bearer <TICKET_AUTH_TOKEN>
//	→ 200 {"ticket": "<PS256 JWT>", "project_id": "...", "expires_at": <unix秒>}
//	→ 401 {"error":"unauthorized"}   → 429 {"error":"rate_limited"}
package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const tokenURI = "https://oauth-login.cloud.huawei.com/oauth2/v3/token" // JWT aud（push-jwt-token 官方模式）

func main() {
	addr := envStr("HOST", "127.0.0.1") + ":" + envStr("PORT", "8091")

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleTicket)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})

	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		mode := "anonymous"
		if os.Getenv("TICKET_AUTH_TOKEN") != "" {
			mode = "token-auth"
		}
		log.Printf("[CF-Ticket] listening on %s (ttl=%ds, %s)", addr, ticketTTLSeconds(), mode)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[CF-Ticket] listen: %v", err)
		}
	}()

	// 优雅关停（SIGINT/SIGTERM；Windows 服务下 NSSM 发 stop 信号）
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("[CF-Ticket] shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// ── 票据签发 handler ──

var issued atomic.Int64 // 累计签发数（日志观察用）。滥用防护不加：token 私有化后无 token 打不进（401 在先），
// 正常负载每部署每 TTL 一签；对齐 CF 滥用响应三件 defer 裁定（2026-08-18 YAGNI，触发=公开推广前）。

// verifyToken 铸票准入（留缝收敛点：鉴权演进只改此函数，handler 零动）。
// 演进路线（2026-08-25 用户定）：
//   现在：匿名开放——TICKET_AUTH_TOKEN 未设=谁都能拿票（测试期，配额可控）。
//   过渡：设 TICKET_AUTH_TOKEN=单 token 鉴权。
//   终态：白名单走云端 txt 实时发布（参考 cloud_function_urls.txt 基建：改 txt 推仓，节点定时拉取
//         热更，VPS+Netlify 共享一份名单）；一户一 token+标签，删行=吊销（旧票活 ≤TTL）。
//         ⚠️ token 真值不能进公开 txt——届时存哈希（比对 sha256）或私有仓 raw，实现期再定。
// who 仅进日志（观察底座），不影响签出的票（票无主体概念，吊销=停发+TTL 自然过期）。
func verifyToken(r *http.Request) (who string, ok bool) {
	tok := os.Getenv("TICKET_AUTH_TOKEN")
	if tok == "" {
		return "anon", true // 匿名开放（当前默认）
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1 {
		return "default", true // 单 token 期无标签；一户一 token 后返真标签
	}
	return "", false
}

func handleTicket(w http.ResponseWriter, r *http.Request) {
	who, ok := verifyToken(r)
	if !ok {
		log.Printf("[CF-Ticket] auth fail")
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	// ⬅ 滥用防护挂点（defer，2026-08-18 裁定 YAGNI；触发=公开推广/漏斗开闸前，届时在此插
	// 限速/封禁返 429，不动 handler 结构）。底座已就位：issued 计数 + issue ok 日志行（按 #/who 可数）。

	acct, err := loadSA()
	if err != nil {
		log.Printf("[CF-Ticket] sa err   %v", err)
		writeJSON(w, 500, map[string]string{"error": "private_json", "msg": err.Error()})
		return
	}
	ttl := time.Duration(ticketTTLSeconds()) * time.Second
	jwt, err := acct.SignJWT(ttl)
	if err != nil {
		log.Printf("[CF-Ticket] sign err %v", err)
		writeJSON(w, 500, map[string]string{"error": "sign", "msg": err.Error()})
		return
	}
	// 日志格式对齐 go-harmony [CF] push ok 行风格（固定动词位+列宽，看着不乱）；无内容可记=天然隐私。
	log.Printf("[CF-Ticket] issue ok  #%-6d ttl=%-4ds proj=%s who=%s", issued.Add(1), int(ttl/time.Second), maskProject(acct.ProjectID), who)
	writeJSON(w, 200, map[string]interface{}{
		"ticket":     jwt,
		"project_id": acct.ProjectID,
		"expires_at": time.Now().Add(ttl).Unix(),
	})
}

// ── service account（自包含：本仓独立于中继仓，SA 加载/签名一份在此）──

// account 华为服务账号（private.json 解析后）。
type account struct {
	KeyID      string `json:"key_id"`
	SubAccount string `json:"sub_account"`
	ProjectID  string `json:"project_id"`
	PrivateKey string `json:"private_key"`
	priv       *rsa.PrivateKey
}

var (
	saOnce sync.Once
	saAcct *account
	saErr  error
)

// loadSA 解析并缓存服务账号（进程一次；fail-fast 校验关键字段）。
func loadSA() (*account, error) {
	saOnce.Do(func() {
		raw := resolvePrivateJSON()
		if raw == "" {
			saErr = fmt.Errorf("PRIVATE_JSON 未找到（env PRIVATE_JSON / PRIVATE_JSON_FILE / 同目录含 PRIVATE KEY 的 .json）")
			return
		}
		var a account
		if e := json.Unmarshal([]byte(raw), &a); e != nil {
			saErr = fmt.Errorf("PRIVATE_JSON 解析失败: %v", e)
			return
		}
		if a.KeyID == "" || a.SubAccount == "" || a.ProjectID == "" {
			saErr = fmt.Errorf("PRIVATE_JSON 缺字段：key_id/sub_account/project_id 任一为空")
			return
		}
		block, _ := pem.Decode([]byte(a.PrivateKey))
		if block == nil {
			saErr = fmt.Errorf("private_key 不是合法 PEM")
			return
		}
		if k, e := x509.ParsePKCS8PrivateKey(block.Bytes); e == nil {
			rk, ok := k.(*rsa.PrivateKey)
			if !ok {
				saErr = fmt.Errorf("PKCS8 私钥非 RSA")
				return
			}
			a.priv = rk
		} else if rk, e := x509.ParsePKCS1PrivateKey(block.Bytes); e == nil {
			a.priv = rk
		} else {
			saErr = fmt.Errorf("解析私钥失败: %v", e)
			return
		}
		saAcct = &a
	})
	return saAcct, saErr
}

// SignJWT 签 PS256 JWT（RSASSA-PSS over SHA-256，salt=digest 长；alg 必须 PS256 非 RS256）。
// JWT 直接当 Bearer 调 push-api，不换 access_token（那是 Connect API 的流程，别串）。
// payload 无 sub claim（照华为官方 5 语言示例 + 实测）。
func (a *account) SignJWT(ttl time.Duration) (string, error) {
	now := time.Now().Unix()
	header := map[string]string{"kid": a.KeyID, "typ": "JWT", "alg": "PS256"}
	payload := map[string]interface{}{"iss": a.SubAccount, "aud": tokenURI, "iat": now, "exp": now + int64(ttl/time.Second)}
	hb, _ := json.Marshal(header)
	pb, _ := json.Marshal(payload)
	signingInput := b64url(hb) + "." + b64url(pb)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPSS(rand.Reader, a.priv, crypto.SHA256, sum[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	if err != nil {
		return "", err
	}
	return signingInput + "." + b64url(sig), nil
}

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// resolvePrivateJSON env > env file > 扫 exe 同目录含 "PRIVATE KEY" 的 .json（认内容不靠字段名）。
func resolvePrivateJSON() string {
	if v := strings.TrimSpace(os.Getenv("PRIVATE_JSON")); v != "" {
		return v
	}
	if f := os.Getenv("PRIVATE_JSON_FILE"); f != "" {
		if b, e := os.ReadFile(f); e == nil {
			return strings.TrimSpace(string(b))
		}
	}
	dir := "."
	if exe, e := os.Executable(); e == nil {
		if d, e2 := filepath.EvalSymlinks(exe); e2 == nil {
			dir = filepath.Dir(d)
		} else {
			dir = filepath.Dir(exe)
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		for _, v := range m {
			if s, ok := v.(string); ok && strings.Contains(s, "PRIVATE KEY") {
				return strings.TrimSpace(string(b))
			}
		}
	}
	return ""
}

// ── 小件 ──

func ticketTTLSeconds() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TICKET_TTL_SECONDS"))); err == nil && v > 0 && v <= 3600 {
		return v
	}
	return 300
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// maskProject 观察日志不打全量 project_id（前 6 位足辨别）。
func maskProject(p string) string {
	if len(p) <= 6 {
		return p
	}
	return p[:6] + "..."
}
