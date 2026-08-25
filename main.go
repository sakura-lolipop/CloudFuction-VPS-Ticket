// CloudFuction-VPS-Ticket —— Hotify CF 2.0 铸票厂（VPS 形态，自包含单二进制 cf-ticket）。
//
// 干一件事：用华为 Service Account 签一张短 exp 的 PS256 JWT（"票"）→ 吐给调用方。
// 不碰任何通知内容（请求体可为空）——调用方（HotifyNEXT-Server）拿票自己直连华为 push-api，
// 通知内容不再过任何自有基础设施（隐私第三层）。设计依据 docs/pushkit-transport.md §9.7。
// 联邦：本仓是公开联邦件，任何人可 clone 部署自己的铸票节点（自己的 SA）。
// 各节点签出的票带各自 project_id，票与节点配套，不可跨节点使用。
//
// 许可策略栈（2026-08-25 设计定稿，env 全默认关 = 纯匿名直通）：
//
//	请求 → 身份二分（token 命中→who:label / 匿名→ip:addr）
//	     → auto-ban 内存临时封（403，固定刑期 TTL 自解；TICKET_AUTO_BAN=0 关）
//	     → 双桶限速（429，IP 桶兜底防多开 + token 桶 per-server；各=0 关）
//	     → 签票
//	永久封 IP = Cloudflare 层（不耗 invoke）；永久封 token = 白名单删行；DoS = CF 限速 + txt 摘节点。
//	（云函数内不设 txt 封禁表：请求到达已耗配额，挡不住 DoS，纯负资产。）
//
// env：
//
//	PRIVATE_JSON / PRIVATE_JSON_FILE  service account key（都不设→扫同目录含 PRIVATE KEY 的 .json）
//	TICKET_AUTH_TOKEN   名单最小载体：设了=单 token 验证（Bearer 匹配→who:default；不匹配→401）；
//	                    不设=匿名开放（当前模式）。终态=云端 txt 白名单热更（sha256+label）。
//	TICKET_TTL_SECONDS  票有效期 1~3600，默认 600（canary 实测 30/300/600 均被华为接受 80000000）
//	TICKET_RATE_LIMIT_IP     IP 桶每分钟张数（宽，防多开兜底）；默认 0=关
//	TICKET_RATE_LIMIT_TOKEN  token 桶每分钟张数（紧，per-server）；默认 0=关
//	TICKET_AUTO_BAN      窗口内撞 429 达 N 次触发临时封（双桶共用 N 各自分记）；默认 0=关
//	TICKET_AUTO_BAN_SECONDS  封多久=strikes 窗口（相等→解封即白纸）；默认 600
//	HOST=127.0.0.1  PORT=8091   （默认只听回环：前面有 Tunnel/nginx 反代；公网直裸再改 0.0.0.0）
//
// 响应契约（调用方 internal/pushkit 按 ticket/project_id/expires_at 三字段消费）：
//
//	GET/POST /
//	→ 200 {"ticket": "<PS256 JWT>", "project_id": "...", "expires_at": <unix秒>}
//	→ 401 {"error":"unauthorized"}  → 403 {"error":"banned"}（Retry-After 头）
//	→ 429 {"error":"rate_limited"}  → 500 {"error":"private_json"|"sign"}
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
	"net"
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

// nowFn 时间源（包级 var：测试可覆盖，对齐 go-harmony harmonyRetryInterval 测试模式）
var nowFn = time.Now

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

// ── 身份二分（许可策略族的单一真相键空间）──

// identity 请求身份。ipKey 恒有（兜底维度）；tokenKey 仅 token 验证通过时有。
type identity struct {
	ipKey    string // "ip:1.2.3.4"
	tokenKey string // "who:default"（token 期名单 label；匿名空）
	label    string // 日志用："anon" / "default"（将来名单真标签）
}

// denied：带了 Bearer 且 TICKET_AUTH_TOKEN 设了但不匹配 → 401（配置错要暴露，不静默降级匿名）。
// 未设 TICKET_AUTH_TOKEN → 一律匿名（当前模式）；匹配 → who:default。
func resolveIdentity(r *http.Request) (identity, bool) {
	id := identity{label: "anon"}
	if ip := clientIP(r); ip != "" {
		id.ipKey = "ip:" + ip
	}
	tok := os.Getenv("TICKET_AUTH_TOKEN")
	if tok == "" {
		return id, true // 匿名开放（当前默认，2026-08-25 裁定）
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if auth == "" {
		return id, true // 设了 token 但请求没带 → 匿名（IP 记账）；单 token 期不强制
	}
	got := strings.TrimPrefix(auth, "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1 {
		id.tokenKey = "who:default" // 一户一 token 后=who:<label>
		id.label = "default"
		return id, true
	}
	return id, false // 带了头但值错 → 401（配置错要暴露，不静默降级匿名）
}

// clientIP 可信 IP：CF-Connecting-IP（Tunnel 覆盖写，客户端伪造不了）→ RemoteAddr。
// 绝不读客户端自带的 X-Forwarded-For。
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

// ── 策略状态（限速桶 + strikes + 封禁表；单实例内存态）──

var (
	guardMu       sync.Mutex
	rateBuckets   map[string][]time.Time // 限速桶：key → 窗口内签发时间戳（"ip:x"/"who:x"）
	strikes       map[string][]time.Time // 撞 429 记录：key → 窗口内被拒时间戳
	bannedUntil   map[string]time.Time   // 临时封：key → 解封时刻（惰性检查，过期即白纸）
)

var issued atomic.Int64 // 累计签发数（日志观察用）+ 滥用观察底座

// bucketAllow 单桶滑动窗口：窗口内未满记一戳返 true；满返 false。
func bucketAllow(buckets map[string][]time.Time, key string, limit int) bool {
	if limit <= 0 {
		return true // 0=该桶关闭
	}
	now := nowFn()
	stamps := buckets[key]
	kept := stamps[:0]
	for _, t := range stamps {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		buckets[key] = kept
		return false
	}
	buckets[key] = append(kept, now)
	return true
}

// addStrike 记一次 429，返回是否触发临时封（窗口内 strikes ≥ N）。
func addStrike(key string, n int, window, banTTL time.Duration) bool {
	if n <= 0 {
		return false // auto-ban 关
	}
	now := nowFn()
	st := strikes[key]
	kept := st[:0]
	for _, t := range st {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	strikes[key] = kept
	if len(kept) >= n {
		bannedUntil[key] = now.Add(banTTL)
		strikes[key] = nil // 触发即清，解封后白纸（固定刑期：期间请求不刷新不记账）
		return true
	}
	return false
}

// banRemain 查封禁（惰性：过期顺手删，返 0=未封）。
func banRemain(key string) time.Duration {
	if until, ok := bannedUntil[key]; ok {
		if d := nowFn().Sub(until); d < 0 {
			return -d
		}
		delete(bannedUntil, key)
	}
	return 0
}

// resetGuard 清策略状态（测试隔离用）。
func resetGuard() {
	guardMu.Lock()
	defer guardMu.Unlock()
	rateBuckets = nil
	strikes = nil
	bannedUntil = nil
}

// ── 票据签发 handler ──

func handleTicket(w http.ResponseWriter, r *http.Request) {
	id, ok := resolveIdentity(r)
	if !ok {
		log.Printf("[CF-Ticket] auth fail")
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}

	guardMu.Lock()
	rateBucketsMap() // 惰性初始化三张表（guardMu 持有下）
	// ① 临时封（双键任一命中；固定刑期，期间请求零记账）
	for _, key := range []string{id.tokenKey, id.ipKey} {
		if key == "" {
			continue
		}
		if d := banRemain(key); d > 0 {
			guardMu.Unlock()
			log.Printf("[CF-Ticket] ban hit  who=%s key=%s retry=%s", id.label, key, d.Truncate(time.Second))
			w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())+1))
			writeJSON(w, 403, map[string]string{"error": "banned"})
			return
		}
	}
	// ② 双桶限速（IP 桶恒查兜底；token 桶有 tokenKey 才查）。超限的桶各自记 strikes。
	ipOver := !bucketAllow(rateBuckets, id.ipKey, ticketRateLimitIP())
	tokOver := id.tokenKey != "" && !bucketAllow(rateBuckets, id.tokenKey, ticketRateLimitToken())
	if ipOver || tokOver {
		banned := ""
		if ipOver && addStrike(id.ipKey, ticketAutoBan(), autoBanWindow(), autoBanTTL()) {
			banned = id.ipKey
		}
		if tokOver && id.tokenKey != "" && addStrike(id.tokenKey, ticketAutoBan(), autoBanWindow(), autoBanTTL()) {
			banned = id.tokenKey
		}
		if banned != "" {
			log.Printf("[CF-Ticket] auto-ban key=%s exp=%s", banned, autoBanTTL().Truncate(time.Second))
		}
		guardMu.Unlock()
		log.Printf("[CF-Ticket] rate lim  who=%s ip=%s", id.label, id.ipKey)
		writeJSON(w, 429, map[string]string{"error": "rate_limited"})
		return
	}
	guardMu.Unlock()

	// ③ 签票
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
	// 日志格式对齐 go-harmony [CF] push ok 行风格（固定动词位+列宽）；无内容可记=天然隐私。
	log.Printf("[CF-Ticket] issue ok  #%-6d ttl=%-4ds proj=%s who=%s", issued.Add(1), int(ttl/time.Second), maskProject(acct.ProjectID), id.label)
	writeJSON(w, 200, map[string]interface{}{
		"ticket":     jwt,
		"project_id": acct.ProjectID,
		"expires_at": nowFn().Add(ttl).Unix(),
	})
}

// rateBucketsMap 惰性初始化（guardMu 持有下调用）。
func rateBucketsMap() map[string][]time.Time {
	if rateBuckets == nil {
		rateBuckets = map[string][]time.Time{}
	}
	if strikes == nil {
		strikes = map[string][]time.Time{}
	}
	if bannedUntil == nil {
		bannedUntil = map[string]time.Time{}
	}
	return rateBuckets
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
	now := nowFn().Unix()
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

// ── env 小件 ──

func ticketTTLSeconds() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TICKET_TTL_SECONDS"))); err == nil && v > 0 && v <= 3600 {
		return v
	}
	return 600
}

func ticketRateLimitIP() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TICKET_RATE_LIMIT_IP"))); err == nil && v > 0 {
		return v
	}
	return 0 // 默认关
}

func ticketRateLimitToken() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TICKET_RATE_LIMIT_TOKEN"))); err == nil && v > 0 {
		return v
	}
	return 0 // 默认关
}

func ticketAutoBan() int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TICKET_AUTO_BAN"))); err == nil && v > 0 {
		return v
	}
	return 0 // 默认关
}

// autoBanTTL 封禁时长=strikes 窗口（相等→解封时旧 strikes 全部出窗=白纸从零开始）。
func autoBanTTL() time.Duration {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TICKET_AUTO_BAN_SECONDS"))); err == nil && v >= 1 {
		return time.Duration(v) * time.Second
	}
	return 600 * time.Second
}

func autoBanWindow() time.Duration { return autoBanTTL() }

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
