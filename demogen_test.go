package main

// demogen：GENDEMO=1 go test -run TestGenerateDemo 生成 demo.html（worktree 根）。
// 用真 consoleHTML 渲造好的假快照 → 资源内联（tablerCSS 内联 <style>、icon 转 data URI）→
// <!--DEMOMOCK--> 注入 fetch mock（?json=1 返回演化假数据，壁纸等真网络请求放行）。
// 只进 worktree 不进 main；页面右上常驻 DEMO 徽章防误当线上。

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"
)

const demoMockJS = `<script>
window.DEMO_MOCK=1;
(function(){
var n=0,total=12847,today=312,up=316*86400+7*3600+42*60+33;
var st={'401':23,'403':2,'429':57,'500':1};
var ips=[{ip:'198.51.100.23',count:4210},{ip:'203.0.113.7',count:2330},{ip:'192.0.2.88',count:1104},
  {ip:'2001:db8::42',count:377},{ip:'198.51.100.201',count:96}];
var lines=[
'2026/08/30 16:42:10 [ticket] 200 0s 198.51.100.23 POST ✓ ticket #12847',
'2026/08/30 16:41:55 [ticket] 429 0s 203.0.113.7 POST ⚠ rate limit',
'2026/08/30 16:41:31 [ticket] 200 0s 192.0.2.88 POST ✓ ticket #12846',
'2026/08/30 16:40:02 [ticket] 401 0s 203.0.113.44 POST ✗ bad auth',
'2026/08/30 16:39:47 [ticket] 200 0s 2001:db8::42 POST ✓ ticket #12845',
'2026/08/30 16:38:12 [ticket] ⚠ auto-ban ip:203.0.113.99 exp=10m0s',
'2026/08/30 16:38:12 [ticket] 403 0s 203.0.113.99 POST 🧊 ban hit retry=10m0s',
'2026/08/30 16:36:58 [ticket] 200 0s 198.51.100.201 POST ✓ ticket #12844',
'2026/08/30 16:35:40 [ticket] 500 2ms 192.0.2.88 POST ✗ sign failed',
'2026/08/30 16:34:11 [ticket] 200 0s 203.0.113.7 POST ✓ ticket #12843'];
var realFetch=window.fetch;
window.fetch=function(url,opt){
  if(String(url).indexOf('json=1')>=0){
    n++;total+=1+Math.floor(Math.random()*3);
    if(Math.random()<0.4)today++;up+=5;
    if(n%3===0)st['429']++;
    if(n%7===0)st['401']++;
    ips[0].count+=2;ips[1].count+=1;
    if(n%5===0){ips[2].count+=7;ips.sort(function(a,b){return b.count-a.count})}
    var bad=n%4===2;
    lines.unshift('2026/08/30 16:4'+n%10+':0'+n%10+' [ticket] '+(bad?'429 0s 203.0.113.7 POST ⚠ rate limit':'200 0s 198.51.100.'+(20+n%60)+' POST ✓ ticket #'+(12847+n)));
    if(lines.length>40)lines.pop();
    var body={uptime:'',uptime_sec:up,issued:total,issued_today:today,status:st,bans:3,
      top_ips:ips,recent:lines.slice(0,25),ttl_seconds:600,mode:'anon',project_id:'hotifynext-prod-123456'};
    return Promise.resolve({ok:true,json:function(){return Promise.resolve(body)}});
  }
  return realFetch?realFetch(url,opt):Promise.reject('offline');
};
var b=document.createElement('div');
b.textContent='DEMO 假数据';
b.style.cssText='position:fixed;left:12px;bottom:12px;z-index:3000;font:11px ui-monospace,monospace;'+
  'padding:3px 10px;border-radius:999px;background:rgba(214,51,108,.9);color:#fff;letter-spacing:.05em';
document.addEventListener('DOMContentLoaded',function(){document.body.appendChild(b)});
})();
</script>`

func TestGenerateDemo(t *testing.T) {
	if os.Getenv("GENDEMO") != "1" {
		t.Skip("GENDEMO=1 才生成 demo.html")
	}
	up := (316*24+7)*time.Hour + 42*time.Minute + 33*time.Second
	snap := statsSnapshot{
		Uptime:      "316 天 7 小时 42 分钟",
		UptimeSec:   int64(up.Seconds()),
		UptimeDur:   up,
		Issued:      12847,
		IssuedToday: 312,
		Status:      map[int]int64{401: 23, 403: 2, 429: 57, 500: 1},
		Bans:        3,
		TopIPs: []statsIPCount{
			{IP: "198.51.100.23", Count: 4210},
			{IP: "203.0.113.7", Count: 2330},
			{IP: "192.0.2.88", Count: 1104},
			{IP: "2001:db8::42", Count: 377},
			{IP: "198.51.100.201", Count: 96},
		},
		Recent: []string{
			"2026/08/30 16:42:10 [ticket] 200 0s 198.51.100.23 POST ✓ ticket #12847",
			"2026/08/30 16:41:55 [ticket] 429 0s 203.0.113.7 POST ⚠ rate limit",
			"2026/08/30 16:41:31 [ticket] 200 0s 192.0.2.88 POST ✓ ticket #12846",
			"2026/08/30 16:40:02 [ticket] 401 0s 203.0.113.44 POST ✗ bad auth",
			"2026/08/30 16:39:47 [ticket] 200 0s 2001:db8::42 POST ✓ ticket #12845",
			"2026/08/30 16:38:12 [ticket] ⚠ auto-ban ip:203.0.113.99 exp=10m0s",
			"2026/08/30 16:38:12 [ticket] 403 0s 203.0.113.99 POST 🧊 ban hit retry=10m0s",
			"2026/08/30 16:36:58 [ticket] 200 0s 198.51.100.201 POST ✓ ticket #12844",
			"2026/08/30 16:35:40 [ticket] 500 2ms 192.0.2.88 POST ✗ sign failed",
			"2026/08/30 16:34:11 [ticket] 200 0s 203.0.113.7 POST ✓ ticket #12843",
		},
		TTL:       600,
		Mode:      "anon",
		ProjectID: "hotifynext-prod-123456",
	}
	// 双语两份：zh→demo.html / en→demo-en.html，语言链互指（静态文件吃不动 ?lang= 服务端重渲染——
	// 互链让 demo 真切语言；两份各走真 consoleHTML(lang) 渲染路径）
	for _, tc := range []struct{ lang, file, linkFrom, linkTo string }{
		{"zh", "demo.html", `href="?lang=en"`, `href="demo-en.html"`},
		{"en", "demo-en.html", `href="?lang=zh"`, `href="demo.html"`},
	} {
		page := consoleHTML(snap, tc.lang, "ticket.hotify.love", "", 5)
		cssLink := `<link rel="stylesheet" href="/tabler.min.css">`
		if !strings.Contains(page, cssLink) {
			t.Fatal("css link 锚点缺失——模板变了要同步 demogen")
		}
		page = strings.Replace(page, cssLink, "<style>\n"+string(tablerCSS)+"\n</style>", 1)
		iconURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(hotifyIcon)
		page = strings.ReplaceAll(page, "/hotify-icon.png", iconURI)
		if !strings.Contains(page, tc.linkFrom) {
			t.Fatalf("%s 语言链锚点 %q 缺失——模板变了要同步 demogen", tc.file, tc.linkFrom)
		}
		page = strings.Replace(page, tc.linkFrom, tc.linkTo, 1)
		if !strings.Contains(page, "<!--DEMOMOCK-->") {
			t.Fatal("<!--DEMOMOCK--> 锚点缺失——模板变了要同步 demogen")
		}
		page = strings.Replace(page, "<!--DEMOMOCK-->", demoMockJS, 1)
		if err := os.WriteFile(tc.file, []byte(page), 0644); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s 生成：%d bytes", tc.file, len(page))
	}
}
