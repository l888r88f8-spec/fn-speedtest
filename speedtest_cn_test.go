package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

const speedtestCNHeader = "id,active,https,cros,preferred,host,country_code,province,city,ver,operator,lon,lat,times,high_speed,sponsor,sponsor_url,current_version,distance,pingUrl,downloadUrl,uploadUrl,websocketUrl\n"

func speedtestCNRow(id, host, province, city, operator, sponsor, base string) string {
	return fmt.Sprintf("%s,1,1,1,0,%s,CN,%s,%s,2,%s,0,0,0,0,%s,https://www.speedtest.cn/,,0,%s/hello,%s/download,%s/upload,%s/ws\n", id, host, province, city, operator, sponsor, base, base, base, base)
}

func TestParseSpeedtestCNCatalogFiltersZhejiangUniversity(t *testing.T) {
	data := speedtestCNHeader +
		speedtestCNRow("1", "node.example:8080", "江苏", "南京", "电信", "南京电信", "https://node.example:8080") +
		speedtestCNRow("2", "speedtest.zju.edu.cn:8080", "浙江", "杭州", "教育网", "浙江大学", "https://speedtest.zju.edu.cn:8080") +
		speedtestCNRow("3", "node.example:8080", "江苏", "南京", "电信", "重复节点", "https://node.example:8080")
	targets, err := parseSpeedtestCNCatalog([]byte(data))
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets=%+v err=%v", targets, err)
	}
	target := targets[0]
	if target.Source.ID != "http:cn:1" || target.Source.Province != "江苏" || target.Source.Carrier != "中国电信" || target.Protocol != "speedtestcn" {
		t.Fatalf("unexpected target: %+v", target)
	}
	if target.Version != "2" || target.CustomURL != "http://node.example:8080/upload.php" {
		t.Fatalf("protocol metadata was not preserved: %+v", target)
	}
	if blockedSpeedtestCNNode("Duke Kunshan University") {
		t.Fatal("non-ZJU university was blocked")
	}
}

func TestSpeedtestCNCarrierRecognizesEducationNetwork(t *testing.T) {
	if got := speedtestCNCarrier("教育网 云测节点"); got != "教育网" {
		t.Fatalf("carrier=%q", got)
	}
}

func mockSpeedtestCN(t *testing.T) (*httptest.Server, httpTarget) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != speedtestCNBrowserUA {
			http.Error(w, "browser user-agent required", http.StatusForbidden)
			return
		}
		if r.URL.Query().Has("r") {
			http.Error(w, "unexpected cache-buster", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/hello":
			http.Redirect(w, r, "/edge/hello", http.StatusTemporaryRedirect)
		case "/edge/hello":
			io.WriteString(w, "hello")
		case "/download":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(make([]byte, 1<<20))
		case "/upload":
			if r.Method != http.MethodPost {
				http.Error(w, "method", http.StatusMethodNotAllowed)
				return
			}
			if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
				http.Error(w, "content type", http.StatusUnsupportedMediaType)
				return
			}
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Error(err)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"success":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	source := httpSource{ID: "http:cn:1", Name: "南京", Sponsor: "中国电信", Province: "江苏", Carrier: "中国电信", Kind: "speedtestcn", Page: "https://www.speedtest.cn/"}
	target := httpTarget{Protocol: "speedtestcn", Source: source, Network: networkIdentity{PublicIP: "114.114.114.114", ISP: "中国电信", Carrier: "中国电信", CountryCode: "CN", Province: "江苏"}, PingURL: server.URL + "/hello", DownloadURL: server.URL + "/download", UploadURL: server.URL + "/upload"}
	return server, target
}

func TestSpeedtestCNClassicProbeUsesECSLayout(t *testing.T) {
	var classicHits, directHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/speedtest/random1000x1000.jpg":
			classicHits++
			_, _ = w.Write([]byte{1})
		case "/download":
			directHits++
			http.Error(w, "direct endpoint should not be primary for v1", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	target := httpTarget{Protocol: "speedtestcn", Version: "1", CustomURL: server.URL + "/upload.php", DownloadURL: server.URL + "/download"}
	if err := speedtestCNQuickDownloadProbe(context.Background(), server.Client(), target); err != nil {
		t.Fatalf("classic ECS probe failed: %v", err)
	}
	if classicHits != 1 || directHits != 0 {
		t.Fatalf("classicHits=%d directHits=%d", classicHits, directHits)
	}
}

func TestSpeedtestCNECSCustomServerPath(t *testing.T) {
	client := newSpeedtestCNCustomClient(profiles["quick"])
	server, err := client.CustomServer("http://node.example:8080/upload.php")
	if err != nil {
		t.Fatal(err)
	}
	if server.URL != "http://node.example:8080/speedtest/upload.php" {
		t.Fatalf("custom server URL=%q", server.URL)
	}
}

func TestSpeedtestCNLatencyFollowsCrossHostRedirectAndKeepsSuccessfulSamples(t *testing.T) {
	var finalCalls int
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != speedtestCNBrowserUA {
			http.Error(w, "browser user-agent required", http.StatusForbidden)
			return
		}
		finalCalls++
		if finalCalls == 1 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "pong")
	}))
	defer final.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/hello", http.StatusFound)
	}))
	defer redirect.Close()

	target := httpTarget{Protocol: "speedtestcn", PingURL: redirect.URL + "/hello"}
	latency, jitter, err := speedtestCNLatency(context.Background(), redirect.Client(), target, nil)
	if err != nil {
		t.Fatalf("latency probe failed: %v", err)
	}
	if latency <= 0 || jitter < 0 {
		t.Fatalf("latency=%f jitter=%f", latency, jitter)
	}
	if finalCalls != 3 {
		t.Fatalf("finalCalls=%d want 3", finalCalls)
	}
}

func TestSpeedtestCNDiscoveryAndSelectedRun(t *testing.T) {
	server, target := mockSpeedtestCN(t)
	server.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if _, _, err := checkHTTPTarget(context.Background(), server.Client(), target, nil); err != nil {
		t.Fatalf("direct probe failed: %v: %v", err, errors.Unwrap(err))
	}
	m := newMultiEngine()
	m.client = server.Client()
	m.sources = nil
	m.cnDirectory = func(context.Context, *http.Client, networkIdentity) ([]httpTarget, error) {
		return []httpTarget{target}, nil
	}
	m.detect = func(context.Context) (*speedtest.User, networkIdentity) {
		return &speedtest.User{IP: target.Network.PublicIP, Isp: target.Network.Carrier}, target.Network
	}
	m.directory = func(context.Context, *http.Client, *speedtest.User, networkIdentity) (serverListResponse, error) {
		return serverListResponse{}, fmt.Errorf("offline")
	}
	list, err := m.Discover(context.Background())
	if err != nil || len(list.Servers) != 1 || list.Servers[0].Kind != "speedtestcn" || !list.Servers[0].ProvinceMatched || !list.Servers[0].CarrierMatched {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	result, err := m.Run(context.Background(), profiles["quick"], target.Source.ID, func(string, int, string) {}, func(liveSample) {})
	if err != nil || result.Engine != "Speedtest.cn" || result.DownloadMbps <= 0 || result.UploadMbps <= 0 || !strings.Contains(result.ServerLocation, "江苏") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestSpeedtestCNHTTPPingFailureFallsBackToTCP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != speedtestCNBrowserUA {
			http.Error(w, "browser user-agent required", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/hello":
			http.Error(w, "ping endpoint unavailable", http.StatusServiceUnavailable)
		case "/download":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(make([]byte, 1<<20))
		case "/upload":
			_, _ = io.Copy(io.Discard, r.Body)
			io.WriteString(w, `{"success":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := httpTarget{
		Protocol: "speedtestcn",
		Source:   httpSource{ID: "http:cn:tcp-fallback", Name: "测试", Sponsor: "中国电信", Province: "江苏", Carrier: "中国电信", Kind: "speedtestcn"},
		PingURL:  server.URL + "/hello", DownloadURL: server.URL + "/download", UploadURL: server.URL + "/upload",
	}
	latency, jitter, err := checkHTTPTarget(context.Background(), server.Client(), target, nil)
	if err != nil {
		t.Fatalf("TCP fallback should keep usable node: %v", err)
	}
	if latency <= 0 || jitter != 0 {
		t.Fatalf("latency=%f jitter=%f", latency, jitter)
	}
}

func TestSpeedtestCNDiscoveryKeepsCandidateWhenAdvisoryProbeFails(t *testing.T) {
	target := httpTarget{
		Protocol: "speedtestcn",
		Source:   httpSource{ID: "http:cn:advisory", Name: "南京", Sponsor: "中国电信", Province: "江苏", Carrier: "中国电信", Kind: "speedtestcn"},
		PingURL:  "http://127.0.0.1:1/hello", DownloadURL: "http://127.0.0.1:1/download", UploadURL: "http://127.0.0.1:1/upload",
	}
	m := newMultiEngine()
	m.client = &http.Client{Timeout: 300 * time.Millisecond}
	m.cnDirectory = func(context.Context, *http.Client, networkIdentity) ([]httpTarget, error) {
		return []httpTarget{target}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out := m.discoverSpeedtestCN(ctx, networkIdentity{CountryCode: "CN", Province: "江苏", Carrier: "中国电信"})
	if len(out.Servers) != 1 {
		t.Fatalf("servers=%+v diagnostic=%+v", out.Servers, out.Diagnostic)
	}
	if out.Servers[0].LatencyMeasured {
		t.Fatalf("unexpected measured latency: %+v", out.Servers[0])
	}
	if !strings.Contains(out.Diagnostic.Message, "延时可测") {
		t.Fatalf("diagnostic=%q", out.Diagnostic.Message)
	}
	m.mu.RLock()
	_, cached := m.targets[target.Source.ID]
	m.mu.RUnlock()
	if !cached {
		t.Fatal("candidate was removed after advisory probe failure")
	}
}

func TestSpeedtestCNFailureSummary(t *testing.T) {
	got := speedtestCNMainFailure(map[string]int{
		"上传验证失败（HTTP 403）": 2,
		"延迟探测失败（连接超时）":     7,
	})
	if got != "延迟探测失败（连接超时） ×7" {
		t.Fatalf("summary=%q", got)
	}
}

func TestSpeedtestCNCandidatesKeepFullCatalogOrder(t *testing.T) {
	mk := func(id, province, carrier string) httpTarget {
		return httpTarget{Source: httpSource{ID: id, Province: province, Carrier: carrier}}
	}
	targets := []httpTarget{mk("other", "四川", "中国移动"), mk("province", "江苏", "中国联通"), mk("carrier", "浙江", "中国电信"), mk("exact", "江苏", "中国电信")}
	selected := speedtestCNCandidates(targets, networkIdentity{CountryCode: "CN", Province: "江苏", Carrier: "中国电信"}, len(targets))
	for i := range targets {
		if selected[i].Source.ID != targets[i].Source.ID {
			t.Fatalf("catalog order changed: got=%+v", selected)
		}
	}
}

func TestLiveSpeedtestCNCatalog(t *testing.T) {
	if os.Getenv("SPEEDTEST_CN_INTEGRATION") != "1" {
		t.Skip("set SPEEDTEST_CN_INTEGRATION=1 to verify the live catalog")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	targets, err := fetchSpeedtestCNCatalogURL(ctx, &http.Client{Timeout: 20 * time.Second}, speedtestCNRawCatalog)
	if err != nil || len(targets) == 0 {
		t.Fatalf("targets=%d err=%v", len(targets), err)
	}
	for _, target := range targets {
		if blockedSpeedtestCNNode(target.Source.Name, target.Source.Sponsor, target.PingURL) {
			t.Fatalf("blocked node passed parser: %+v", target.Source)
		}
	}
	t.Logf("parsed %d eligible live catalog nodes", len(targets))
}


func TestRankServerOptionsHealthThenLatency(t *testing.T) {
	list := serverListResponse{Servers: []serverOption{
		{ID: "failed-fast", HealthStatus: "failed", LatencyMS: 2},
		{ID: "unknown-20", LatencyMS: 20},
		{ID: "success-30", HealthStatus: "success", LatencyMS: 30},
		{ID: "unknown-5", LatencyMS: 5},
	}}
	rankServerOptions(&list)
	want := []string{"success-30", "unknown-5", "unknown-20", "failed-fast"}
	for i, id := range want {
		if list.Servers[i].ID != id {
			t.Fatalf("order=%+v", list.Servers)
		}
	}
}


func TestBundledSpeedtestGoCLIResult(t *testing.T) {
	dir := t.TempDir()
	bin := dir + "/speedtest-go"
	script := "#!/bin/sh\ncat <<'EOF'\n{\"servers\":[{\"latency\":12000000,\"jitter\":1000000,\"dl_speed\":12500000,\"ul_speed\":6250000}]}\nEOF\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FNOS_SPEEDTEST_GO_BIN", bin)
	target := httpTarget{
		Protocol: "speedtestcn", Version: "1", CustomURL: "http://node.example:8080/upload.php",
		Source: httpSource{ID: "http:cn:cli", Name: "苏州", Sponsor: "教育网", Province: "江苏", Carrier: "教育网", Kind: "speedtestcn"},
		Network: networkIdentity{PublicIP: "1.2.3.4", ISP: "教育网", Carrier: "教育网"},
	}
	result, err := runSpeedtestCNCLI(context.Background(), target, profiles["quick"], func(string, int, string) {}, func(liveSample) {})
	if err != nil {
		t.Fatal(err)
	}
	if result.DownloadMbps != 100 || result.UploadMbps != 50 || result.LatencyMS != 12 || result.JitterMS != 1 {
		t.Fatalf("result=%+v", result)
	}
}


func TestSpeedtestLibraryVersion(t *testing.T) {
	if got := speedtest.Version(); got != "1.8.3" {
		t.Fatalf("speedtest-go library version=%q want 1.8.3", got)
	}
}
