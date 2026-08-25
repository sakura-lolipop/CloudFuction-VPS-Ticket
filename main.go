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
//	永久封 IP = Cloudflare 层（不耗 invoke）；DoS = CF 限速 + txt 摘节点。滥用治理=速率维度（限速/auto-ban/CF），非身份准入。
//	（云函数内不设 txt 封禁表：请求到达已耗配额，挡不住 DoS，纯负资产。）
//
// env：
//
//	PRIVATE_JSON / PRIVATE_JSON_FILE  service account key（都不设→扫同目录含 PRIVATE KEY 的 .json）
//	TICKET_AUTH_TOKEN   名单最小载体：设了=单 token 验证（Bearer 匹配→who:default；不匹配→401）；
//	                    不设=匿名开放（产品形态：默认开放，滥用走速率治理；白名单要做再议——缝见 whitelist.md）。
//	TICKET_TTL_SECONDS  票有效期 1~3600，默认 600（canary 实测 30/300/600 均被华为接受 80000000）
//	TICKET_RATE_LIMIT_IP     IP 桶每分钟张数（宽，防多开兜底）；默认 0=关
//	TICKET_RATE_LIMIT_TOKEN  token 桶每分钟张数（紧，per-server）；默认 0=关
//	TICKET_AUTO_BAN      窗口内撞 429 达 N 次触发临时封（双桶共用 N 各自分记）；默认 0=关
//	TICKET_AUTO_BAN_SECONDS  封多久=strikes 窗口（相等→解封即白纸）；默认 600
//	HOST=127.0.0.1  PORT=12346   （默认只听回环：前面有 Tunnel/nginx 反代；公网直裸再改 0.0.0.0）
//
// 响应契约（调用方 internal/pushkit 按 ticket/project_id/expires_at 三字段消费）：
//
//	POST /ticket（GET /ticket 说明页；根路径 404=扫描器隐身）
//	→ 200 {"ticket": "<PS256 JWT>", "project_id": "...", "expires_at": <unix秒>}（POST /ticket）
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
	"io"
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
	ensureConfigFile() // 首次启动生成带注释的默认 config.yml（自文档）
	port := orDefault(cfgGet("port"), "12346")
	if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
		port = "12346" // port: 0/垃圾静默绑随机端口的坑（对抗审 F），拒之回落默认
	}
	addr := orDefault(cfgGet("host"), "127.0.0.1") + ":" + port // 静态：改 config.yml 的 host/port 要重启

	// 三腿（对齐 NEXT-Server log.md 模式）：stdout 着色渲染（TTY 才开色）+ logs\ticket.log 原样
	// 纯文本（ANSI 不落盘、grep 友好）+ ring（/console 面板黑窗回放）。文件腿打不开退 stdout-only。
	// 锚 exe 目录（与 SA 扫描同根——CWD 跟启动方式走的"改配置无效"族根治，对抗审 P1）。
	logOutput := io.Writer(io.MultiWriter(NewColorWriter(os.Stdout, ColorEnabled()), ringWriter{}))
	logPath := filepath.Join(exeDir(), "logs", "ticket.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err == nil {
		if f, err := openLogRotate(logPath); err == nil {
			logOutput = io.MultiWriter(logOutput, f)
		} else {
			log.Printf("[ticket] ⚠ open %s failed: %v (stdout only)", logPath, err)
		}
	} else {
		log.Printf("[ticket] ⚠ mkdir %s failed: %v (stdout only)", filepath.Dir(logPath), err)
	}
	log.SetOutput(logOutput)

	mux := http.NewServeMux()
	mux.HandleFunc("/ticket", handleTicket) // 根路径不挂=404（扫描器 GET / 得"无内容"归档走人，比说明页更隐身+更省）
	mux.HandleFunc("/console", handleConsole)
	mux.HandleFunc("/tabler.min.css", handleConsoleCSS)
	mux.HandleFunc("/hotify-icon.png", handleConsoleIcon)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		// SA 探针（对抗审 P1：假绿探针——SA 坏是唯一"活着但残废"的故障，health 必须看见）。
		// loadSA 有 sync.Once 缓存，零成本。
		ok, proj := true, "-"
		if acct, err := loadSA(); err != nil {
			ok = false
		} else {
			proj = acct.ProjectID
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":%v,"proj":%q}`+"\n", ok, proj)
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		mode := "anonymous"
		if ticketAuthToken() != "" {
			mode = "token-auth"
		}
		acct, err := loadSA()
		proj := "-"
		if err == nil {
			proj = acct.ProjectID
		}
		log.Printf("[ticket] listen %s (ttl=%ds proj=%s %s)", addr, ticketTTLSeconds(), proj, mode)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[ticket] listen: %v", err)
		}
	}()

	// 优雅关停（SIGINT/SIGTERM；Windows 服务下 NSSM 发 stop 信号）
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("[ticket] shutdown")
	shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// exeDir exe 所在目录（EvalSymlinks 容错；失败回落 "."）。config/logs 锚此——与 SA 扫描同根。
func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if d, err := filepath.EvalSymlinks(exe); err == nil {
		return filepath.Dir(d)
	}
	return filepath.Dir(exe)
}

// openLogRotate 打开日志文件（带单代轮转：超 10MB 时 rename 成 .1 重开——P0 对抗审：bot 探测
// 每行 90-120B，无上限最坏 GB 级/年盘满杀同机全家，且 Windows 运行中锁文件没法手动清）。
func openLogRotate(path string) (*os.File, error) {
	const maxSize = 10 << 20 // 10MB
	if fi, err := os.Stat(path); err == nil && fi.Size() >= maxSize {
		_ = os.Remove(path + ".1")         // 保留一代（旧 .1 让位）
		_ = os.Rename(path, path+".1")     // rename 失败（极罕见）则继续追加原文件，不挡服务
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
}

// reqLog 请求行平铺日志（渲染层按 [ticket]+首4token 列化）+ /stats 记账（单一真相：所有请求
// 终态必经此，漏一个分支=面板少一类数）。[ticket] <status> <dur> <ip> <method> <body>。
func reqLog(r *http.Request, status int, start time.Time, body string) {
	log.Printf("[ticket] %d %s %s %s %s", status, nowFn().Sub(start), clientIP(r), r.Method, body)
	statsRecord(status, clientIP(r), status == http.StatusOK)
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
	tok := ticketAuthToken()
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

// resetGuard 清策略状态（测试隔离用；顺带隔离 config 缓存）。
func resetGuard() {
	guardMu.Lock()
	defer guardMu.Unlock()
	rateBuckets = nil
	strikes = nil
	bannedUntil = nil
	cfgReset()
}

// ── 票据签发 handler ──

func handleTicket(w http.ResponseWriter, r *http.Request) {
	// 对抗审 P2：路径精确匹配 + method 白名单——catch-all 让 bot 任意探测路径全 200（RSA 白签
	// +日志放大+鼓励枚举）。签票口就是根路径，GET/POST/HEAD。
	if r.URL.Path != "/ticket" {
		writeJSON(w, 404, map[string]string{"error": "not found"})
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// GET=说明页（零 RSA）：爬虫/浏览器直开不再白烧签名（卡巴 cerebro 扫新域名事件，
		// 2026-08-25）。取凭证是动作，POST 才签——REST 正确形态。
		writeJSON(w, 200, map[string]string{
			"service": "hotify-ticket",
			"hint":    "POST / to get a ticket {ticket, project_id, expires_at}",
			"console": "/console",
			"health":  "/health",
		})
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	start := nowFn()
	id, ok := resolveIdentity(r)
	if !ok {
		reqLog(r, 401, start, "✗ auth fail")
		writeJSON(w, 401, map[string]string{"error": "unauthorized"})
		return
	}
	// who 非匿名才尾缀 body（匿名期 who=anon 零信息，省略；token 期=归因锚）
	who := func(body string) string {
		if id.label != "anon" {
			return body + " who=" + id.label
		}
		return body
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
			reqLog(r, 403, start, who("🧊 ban hit retry="+d.Truncate(time.Second).String()))
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
			log.Printf("[ticket] ⚠ auto-ban %s exp=%s", banned, autoBanTTL().Truncate(time.Second)); statsRecordBan()
		}
		guardMu.Unlock()
		reqLog(r, 429, start, who("⚠ rate limit"))
		writeJSON(w, 429, map[string]string{"error": "rate_limited"})
		return
	}
	guardMu.Unlock()

	// ③ 签票
	acct, err := loadSA()
	if err != nil {
		reqLog(r, 500, start, "✗ sa error: "+err.Error())
		writeJSON(w, 500, map[string]string{"error": "private_json", "msg": err.Error()})
		return
	}
	ttl := time.Duration(ticketTTLSeconds()) * time.Second
	jwt, err := acct.SignJWT(ttl)
	if err != nil {
		reqLog(r, 500, start, "✗ sign error: "+err.Error())
		writeJSON(w, 500, map[string]string{"error": "sign", "msg": err.Error()})
		return
	}
	// body 纯动作短语（归因靠列：状态/耗时/IP/method；proj/ttl 恒定值只在 listen 行）
	reqLog(r, 200, start, who(fmt.Sprintf("✓ ticket #%d", issued.Add(1))))
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

// ── 配置小件（config.yml > env > 默认，见 config.go；除 port/host 外全热加载）──

func ticketTTLSeconds() int {
	if v := cfgInt("ttl", 600, 3600); v >= 1 {
		return v
	}
	return 600 // ttl:0 → 票出生即过期（exp==iat 华为必拒、服务侧全绿）——下限 1 无人认领即回落（对抗审 F1）
}

func ticketRateLimitIP() int    { return cfgInt("rate_limit_ip", 0, 0) }
func ticketRateLimitToken() int { return cfgInt("rate_limit_token", 0, 0) }
func ticketAutoBan() int        { return cfgInt("auto_ban", 0, 0) }

// autoBanTTL 封禁时长=strikes 窗口（相等→解封时旧 strikes 全部出窗=白纸从零开始）。
func autoBanTTL() time.Duration {
	v := cfgInt("auto_ban_seconds", 600, 0)
	if v < 1 {
		v = 600
	}
	return time.Duration(v) * time.Second
}

func autoBanWindow() time.Duration { return autoBanTTL() }

// ticketAuthToken 铸票鉴权 token（空=匿名开放）。resolveIdentity/console 共用。
func ticketAuthToken() string { return cfgGet("auth_token") }

// orDefault s 非空返 s 否则 def。
func orDefault(s, def string) string {
	if strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
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
