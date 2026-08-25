package main

// 联邦账本（cloudfuctiontd.md plan v2.1，2026-08-25 定稿后动工一半 DEFER 的 WIP）。
// ⚠️ WIP 状态：框架完整（ingest/fedState/轮询/快照），三处 TODO 未完成（见下），
// 编译应过、无测试、未接线 main。恢复实施时按 docs/cloudfuctiontd.md plan v2.1 六步走。
//
// 设计：云函数不落盘，VPS 代记——
// 通道 A（实时行·尽力而为）：节点每请求 POST /ingest 一行（采样，Serverless 冻结可丢）。
// 通道 B（聚合·可靠）：health 轮询（5min）时节点捎带实例计数 {_iid,since,n,counts,top_ips}，
//   per-instance 差值累加（_iid→last，差值入账；_iid 更换=新实例从零）——防重启重复计数。
// 节点自动发现：ingest 上报即出现（node=完整端点 URL 自报不验证——观察数据无安全面）。

import (
	"context"
	"log"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fedRingMax     = 200              // 每节点 A 通道行窗口
	fedHealthSlots = 60               // 健康点阵（5min×60=5h 窗口）
	fedPollEvery   = 5 * time.Minute  // 轮询间隔（60s=保活白烧 invokes，见 td 账）
)

type fedNode struct {
	mu        sync.Mutex
	kind      string          // "cloud-function" 等（自报；☁ 徽章判据）
	counts    map[int]int64   // B 通道聚合累计（差值累加后）
	instCount map[string]int64 // _iid → 上次上报 n（总量差值基准）
	instCounts map[string]map[int]int64 // TODO② _iid → 上次各码计数快照（逐码差值用，未实现）
	ips       map[string]int64
	ring      []string // A 通道行（最新在末尾）
	lastSeen  time.Time
	health    []bool    // 最近在后；true=up
	lastDown  time.Time
}

var (
	fedMu    sync.Mutex
	fedNodes = map[string]*fedNode{}
)

func fedGet(node string) *fedNode {
	fedMu.Lock()
	defer fedMu.Unlock()
	fn := fedNodes[node]
	if fn == nil {
		fn = &fedNode{
			kind: "?", counts: map[int]int64{}, instCount: map[string]int64{},
			instCounts: map[string]map[int]int64{}, ips: map[string]int64{},
		}
		fedNodes[node] = fn
	}
	return fn
}

// fedIngestLine 通道 A：记一行。
func fedIngestLine(node, kind, status, ip string) {
	fn := fedGet(node)
	line := time.Now().Format("2006/01/02 15:04:05") + " " + ip + " " + status
	fn.mu.Lock()
	if kind != "" && kind != "?" {
		fn.kind = kind
	}
	fn.ring = append(fn.ring, line)
	if len(fn.ring) > fedRingMax {
		fn.ring = fn.ring[len(fn.ring)-fedRingMax:]
	}
	fn.lastSeen = time.Now()
	fn.mu.Unlock()
}

// handleIngest POST {node, kind, status, ip}——宽限速 60/min/IP（复用限速桶；防刷，无安全面）。
// TODO③ 接线：main.go mux.HandleFunc("/ingest", handleIngest)。
func handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	guardMu.Lock()
	rateBucketsMap()
	allowed := bucketAllow(rateBuckets, "ip:"+clientIP(r), 60)
	guardMu.Unlock()
	if !allowed {
		writeJSON(w, 429, map[string]string{"error": "rate_limited"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<13)) // 8KB 上限
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad body"})
		return
	}
	var in struct {
		Node   string `json:"node"`
		Kind   string `json:"kind"`
		Status string `json:"status"`
		IP     string `json:"ip"`
	}
	if json.Unmarshal(body, &in) != nil || in.Node == "" || !strings.HasPrefix(in.Node, "https://") {
		writeJSON(w, 400, map[string]string{"error": "bad json (node=https endpoint url required)"})
		return
	}
	fedIngestLine(in.Node, in.Kind, in.Status, in.IP)
	log.Printf("[ticket] fed %s %s %s %s", shortNode(in.Node), in.IP, orDefault(in.Kind, "?"), in.Status)
	writeJSON(w, 200, map[string]string{"ok": "true"})
}

// fedHealth 自报结构（通道 B 捎带，Netlify health.js 等实现侧产出）。
type fedHealthReport struct {
	OK     bool             `json:"ok"`
	Proj   string           `json:"proj"`
	IID    string           `json:"_iid"`
	Since  int64            `json:"since"`
	N      int64            `json:"n"`
	Counts map[string]int64 `json:"counts"`
	TopIPs map[string]int64 `json:"top_ips"`
}

// startFedPoller 后台健康轮询（5min×远端节点）+ B 通道聚合计账。
// TODO③ 接线：main() 里 startFedPoller(ctx)。
func startFedPoller(ctx context.Context) {
	go func() {
		client := &http.Client{Timeout: 8 * time.Second}
		tick := time.NewTicker(fedPollEvery)
		defer tick.Stop()
		poll := func() {
			fedMu.Lock()
			nodes := make([]string, 0, len(fedNodes))
			for n := range fedNodes {
				nodes = append(nodes, n)
			}
			fedMu.Unlock()
			for _, node := range nodes {
				pollOne(client, node)
			}
		}
		poll() // 启动立即一轮
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				poll()
			}
		}
	}()
}

func pollOne(client *http.Client, endpoint string) {
	healthURL := strings.TrimSuffix(endpoint, "/ticket") + "/health"
	fn := fedGet(endpoint)
	resp, err := client.Get(healthURL)
	up := err == nil && resp.StatusCode == 200
	var rep fedHealthReport
	if up {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if json.Unmarshal(body, &rep) != nil {
			up = false
		}
	}
	fn.mu.Lock()
	fn.health = append(fn.health, up)
	if len(fn.health) > fedHealthSlots {
		fn.health = fn.health[len(fn.health)-fedHealthSlots:]
	}
	if up {
		fn.lastSeen = time.Now()
		// TODO② B 通道差值累加——正确算法（plan v2.1 §6）：
		//   逐码：last := instCounts[iid][code]; delta := rep.counts[code]-last; counts[code]+=delta
		//   _iid 不在表 = 新实例，基准全 0；实例死亡未捎带尾数丢=采样语义。
		//   当前存根只记总量基准（instCount），逐码快照（instCounts）字段已备逻辑未写。
		if rep.IID != "" && rep.N >= fn.instCount[rep.IID] {
			fn.instCount[rep.IID] = rep.N
		}
	} else {
		fn.lastDown = time.Now()
	}
	fn.mu.Unlock()
}

// fedSnapshot 面板/JSON 用快照。
type fedSnapshot struct {
	Node     string         `json:"node"`
	Kind     string         `json:"kind"`
	Counts   map[int]int64  `json:"counts"`
	TopIPs   []statsIPCount `json:"top_ips"`
	Ring     []string       `json:"ring"`
	LastSeen string         `json:"last_seen"`
	Health   []bool         `json:"health"`
	Up       bool           `json:"up"`
}

func takeFedSnapshot() []fedSnapshot {
	fedMu.Lock()
	keys := make([]string, 0, len(fedNodes))
	for k := range fedNodes {
		keys = append(keys, k)
	}
	fedMu.Unlock()
	sort.Strings(keys)
	out := make([]fedSnapshot, 0, len(keys))
	for _, k := range keys {
		fn := fedNodes[k]
		fn.mu.Lock()
		ips := make([]statsIPCount, 0, len(fn.ips))
		for ip, n := range fn.ips {
			ips = append(ips, statsIPCount{ip, n})
		}
		sort.Slice(ips, func(i, j int) bool { return ips[i].Count > ips[j].Count })
		if len(ips) > 10 {
			ips = ips[:10]
		}
		up := len(fn.health) > 0 && fn.health[len(fn.health)-1]
		out = append(out, fedSnapshot{
			Node: k, Kind: fn.kind, Counts: fn.counts, TopIPs: ips,
			Ring: append([]string(nil), fn.ring...),
			LastSeen: humanDur(time.Since(fn.lastSeen), "zh"),
			Health:  append([]bool(nil), fn.health...), Up: up,
		})
		fn.mu.Unlock()
	}
	return out
}

func shortNode(node string) string {
	if u, err := url.Parse(node); err == nil && u.Host != "" {
		return u.Host
	}
	return node
}

// _ 保 strconv 引用（TODO② 逐码差值会用到 strconv.Atoi(normalizeCounts 的码键)；框架期防 unused）
var _ = strconv.Itoa
