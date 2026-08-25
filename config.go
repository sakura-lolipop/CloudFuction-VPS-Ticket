package main

// config.yml 配置层（2026-08-25 加，根治"bat→env 传递链脆"：echo 硬编失真 / set 被乱码破坏 /
// zip 版本漂移都从这来）。对齐 go-harmony config.json 先例：与 exe 同目录、不存在自动生成带
// 注释默认件、mtime 惰性热加载（改完即生效不重启——限速收紧是应急操作，重启会断在途签票）。
//
// 优先级：config.yml > env > 代码默认（对齐 go-harmony）。
// 形态 yml 但**手写极简 kv 解析**（`key: value` 行 + # 注释）——引 yaml.v3 会破单二进制零依赖
// 原则，我们的字段全是标量不需要真 yaml。
//
// port/host 是静态值（监听绑定，改了要重启——不该热加载）；其余全部热加载。

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// configFile 配置文件名（var 非 const：测试指到不存在路径，隔离 cwd 残留 config.yml 压 env）。
var configFile = "config.yml"

// 默认件内容（首次启动生成；注释=自带文档）。
const configDefault = `# Hotify CF-Ticket 配置（改完即生效，热加载；port/host 除外需重启）
# 优先级：本文件 > 环境变量 > 代码默认。删行=回落下一级。
host: 127.0.0.1        # 监听地址（默认只听回环，Tunnel 反代进来）
port: 12346            # 监听端口（需重启生效）
ttl: 600               # 票有效期秒 1~3600（canary 实测 30/300/600 华为全收）
auth_token:            # 铸票鉴权；空=匿名开放（当前模式）
rate_limit_ip: 0       # IP 桶每分钟张数（宽兜底防多开）；0=关
rate_limit_token: 0    # token 桶每分钟张数（紧 per-server）；0=关
auto_ban: 0            # 窗口内撞 429 达 N 次自动临时封；0=关
auto_ban_seconds: 600  # 封多久=strikes 窗口（相等→解封即白纸）
`

var (
	cfgMu    sync.Mutex
	cfgVals  map[string]string
	cfgMtime time.Time
	cfgLoaded bool
)

// cfgGet 取配置：config.yml > env > ""（返空串=调用方走默认）。
// 每次调用 stat 检查 mtime，变了重读（惰性热加载；stat 微秒级，调用频率=每请求）。
func cfgGet(key string) string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if fi, err := os.Stat(configFile); err == nil {
		if !cfgLoaded || fi.ModTime() != cfgMtime {
			if b, err := os.ReadFile(configFile); err == nil {
				cfgVals = parseSimpleYAML(string(b))
				cfgMtime = fi.ModTime()
				cfgLoaded = true
			}
		}
	}
	if cfgVals != nil {
		if v, ok := cfgVals[key]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return os.Getenv(cfgEnvName(key))
}

// cfgEnvName yml key → env 名（port→PORT、rate_limit_ip→TICKET_RATE_LIMIT_IP）。
func cfgEnvName(key string) string {
	switch key {
	case "host", "port":
		return strings.ToUpper(key)
	default:
		return "TICKET_" + strings.ToUpper(key)
	}
}

// ensureConfigFile 首次启动：无 config.yml 则生成默认件（带注释=自文档）。
func ensureConfigFile() {
	if _, err := os.Stat(configFile); err != nil {
		_ = os.WriteFile(configFile, []byte(configDefault), 0644)
	}
}

// parseSimpleYAML 极简 kv：`key: value` 行，# 起注释（值本身不含 #——标量配置够用），空行跳过。
// 修复教训：trim 后的 `auth_token:` 空值行，行尾注释顶头 #（" #" 匹配不到）会被当值——
// 注释剥离必须在原始串上做（首个 # 起），再 trim。
func parseSimpleYAML(content string) map[string]string {
	vals := map[string]string{}
	for _, ln := range strings.Split(content, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if i := strings.Index(ln, ":"); i > 0 {
			k := strings.TrimSpace(ln[:i])
			v := ln[i+1:]
			if j := strings.Index(v, "#"); j >= 0 {
				v = v[:j] // # 起=注释（含空值行 `key: # comment` → v=""）
			}
			v = strings.TrimSpace(v)
			if k != "" {
				vals[k] = v
			}
		}
	}
	return vals
}

// cfgReset 测试隔离：清缓存 + 指到不存在的文件（cwd 若有 config.yml 不会压掉测试 env）。
func cfgReset() {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfgVals = nil
	cfgMtime = time.Time{}
	cfgLoaded = false
	configFile = "config.test-nonexistent.yml"
}

// cfgInt cfgGet 的 int 版（空/垃圾值返 def）。
func cfgInt(key string, def int, max int) int {
	raw := cfgGet(key)
	if raw == "" {
		return def
	}
	if v, err := strconv.Atoi(raw); err == nil {
		if max > 0 && v > max {
			return max
		}
		if v >= 0 {
			return v
		}
	}
	return def
}
