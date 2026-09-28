package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

type fakeRunner struct {
	result       testResult
	err          error
	wait         bool
	wantServerID string
	stopDelay    time.Duration
}

func (f fakeRunner) Run(ctx context.Context, _ profile, serverID string, progress progressFunc, sample sampleFunc) (testResult, error) {
	if f.wantServerID != "" && serverID != f.wantServerID {
		return testResult{}, errors.New("unexpected server id")
	}
	progress("detecting", 6, "正在识别网络")
	latency := 8.2
	sample(liveSample{LatencyMS: &latency})
	if f.wait {
		download, upload := 512.34, 66.78
		downProgress, upProgress := 42, 18
		sample(liveSample{DownloadMbps: &download, UploadMbps: &upload, DownloadPercent: &downProgress, UploadPercent: &upProgress})
		<-ctx.Done()
		if f.stopDelay > 0 {
			time.Sleep(f.stopDelay)
		}
		return testResult{}, ctx.Err()
	}
	progress("selecting", 16, "正在选择节点")
	progress("latency", 31, "正在测量延迟")
	progress("download", 45, "正在测量下载")
	progress("upload", 73, "正在测量上传")
	return f.result, f.err
}

type fakeDiscoverer struct {
	response serverListResponse
	err      error
}

func (f fakeDiscoverer) Discover(context.Context) (serverListResponse, error) {
	return f.response, f.err
}

func TestAPIFlowAndEmbeddedUI(t *testing.T) {
	dataDir := t.TempDir()
	app := newApplication(dataDir)
	app.runner = fakeRunner{wantServerID: "54321", result: testResult{
		LatencyMS: 8.2, JitterMS: 0.7, DownloadMbps: 928.4, UploadMbps: 112.5,
		ServerLocation: "上海（中国）", ServerSponsor: "China Unicom", ServerID: "12345",
		ISP: "China Unicom", PublicIP: "203.0.113.8", CarrierMatched: true,
	}}
	app.discoverer = fakeDiscoverer{response: serverListResponse{
		ISP: "China Unicom", PublicIP: "203.0.113.8", RecommendedID: "54321",
		Servers: []serverOption{{ID: "54321", Name: "上海", Sponsor: "China Unicom", LatencyMS: 8.2, Recommended: true}},
	}}
	server := httptest.NewServer(app.routes())
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/info")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("info endpoint: %v status=%v", err, resp.StatusCode)
	}
	var info map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info["engine"] != "Speedtest.cn + Speedtest.net + HTTP" || info["version"] != appVersion {
		t.Fatalf("unexpected info: %#v", info)
	}

	resp, err = http.Get(server.URL + "/api/history")
	if err != nil {
		t.Fatal(err)
	}
	emptyHistory, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.TrimSpace(string(emptyHistory)) != "[]" {
		t.Fatalf("empty history must be an array, got %s", emptyHistory)
	}

	resp, err = http.Get(server.URL + "/api/servers")
	if err != nil {
		t.Fatal(err)
	}
	var discovered serverListResponse
	_ = json.NewDecoder(resp.Body).Decode(&discovered)
	resp.Body.Close()
	if discovered.RecommendedID != "54321" || len(discovered.Servers) != 1 {
		t.Fatalf("unexpected nearby servers: %#v", discovered)
	}

	resp, err = http.Post(server.URL+"/api/test", "application/json", strings.NewReader(`{"profile":"standard"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("test without a selected server must be rejected: status=%d", resp.StatusCode)
	}

	resp, err = http.Get(server.URL + basePath + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "NAS 网络测速") || !strings.Contains(string(body), "downloadProgressBar") || !strings.Contains(string(body), "uploadProgressBar") || strings.Contains(string(body), "progressWrap") || !strings.Contains(string(body), "cnServerSelect") || !strings.Contains(string(body), "netServerSelect") {
		t.Fatalf("embedded UI not served: status=%d", resp.StatusCode)
	}

	payload, _ := json.Marshal(map[string]string{"profile": "standard", "serverId": "54321"})
	resp, err = http.Post(server.URL+"/api/test", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start status=%d", resp.StatusCode)
	}

	deadline := time.Now().Add(3 * time.Second)
	completed := false
	for time.Now().Before(deadline) {
		resp, err = http.Get(server.URL + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		var state testState
		_ = json.NewDecoder(resp.Body).Decode(&state)
		resp.Body.Close()
		if state.Status == "complete" {
			completed = true
			if state.Result == nil || state.Result.ServerSponsor != "China Unicom" || !state.Result.CarrierMatched {
				t.Fatalf("invalid result: %#v", state.Result)
			}
			if state.Result.ID == "" || state.Result.Timestamp == "" || state.Result.Profile != "standard" {
				t.Fatalf("missing result metadata: %#v", state.Result)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !completed {
		t.Fatal("test did not complete")
	}

	reloaded := newApplication(dataDir)
	if len(reloaded.history) != 1 || reloaded.history[0].ISP != "China Unicom" {
		t.Fatalf("history was not persisted: %#v", reloaded.history)
	}
}

func TestCancel(t *testing.T) {
	app := newApplication(t.TempDir())
	app.runner = fakeRunner{wait: true, stopDelay: 150 * time.Millisecond}
	server := httptest.NewServer(app.routes())
	defer server.Close()

	resp, err := http.Post(server.URL+"/api/test", "application/json", strings.NewReader(`{"profile":"quick","serverId":"123"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start status=%d", resp.StatusCode)
	}

	deadline := time.Now().Add(time.Second)
	liveSeen := false
	for time.Now().Before(deadline) {
		app.mu.RLock()
		liveSeen = app.state.LiveDownloadMbps == 512.34 && app.state.LiveUploadMbps == 66.78 && app.state.LiveLatencyMS == 8.2 && app.state.DownloadProgress == 42 && app.state.UploadProgress == 18
		app.mu.RUnlock()
		if liveSeen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !liveSeen {
		t.Fatal("live speed samples were not exposed in test state")
	}

	resp, err = http.Post(server.URL+"/api/test/cancel", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel status=%d", resp.StatusCode)
	}
	lateDownload, lateUpload, latePercent := 999.0, 999.0, 100
	app.setState("upload", 96, "不应恢复测速")
	app.setLive(liveSample{DownloadMbps: &lateDownload, UploadMbps: &lateUpload, DownloadPercent: &latePercent, UploadPercent: &latePercent})

	app.mu.RLock()
	statusAfterCancel := app.state.Status
	downloadAfterCancel := app.state.DownloadProgress
	uploadAfterCancel := app.state.UploadProgress
	app.mu.RUnlock()
	if statusAfterCancel != "cancelling" || downloadAfterCancel != 42 || uploadAfterCancel != 18 {
		t.Fatalf("cancel must stop UI state immediately: status=%s download=%d upload=%d", statusAfterCancel, downloadAfterCancel, uploadAfterCancel)
	}

	resp, err = http.Post(server.URL+"/api/test", "application/json", strings.NewReader(`{"profile":"quick","serverId":"123"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("new test must be blocked while old task is stopping: status=%d", resp.StatusCode)
	}

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		app.mu.RLock()
		status := app.state.Status
		historyCount := len(app.history)
		app.mu.RUnlock()
		if status == "cancelled" {
			if historyCount != 0 {
				t.Fatal("cancelled test must not be saved to history")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("test did not enter cancelled state")
}

func TestCarrierAwareServerSelection(t *testing.T) {
	user := &speedtest.User{Isp: "China Unicom Beijing"}
	servers := speedtest.Servers{
		{ID: "1", Sponsor: "Independent IDC", Latency: 10 * time.Millisecond, Distance: 10},
		{ID: "2", Sponsor: "China Unicom", Latency: 11 * time.Millisecond, Distance: 20},
	}
	selected, matched, err := selectBestServer(user, servers)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "2" || !matched {
		t.Fatalf("carrier-aware selection failed: id=%s matched=%v", selected.ID, matched)
	}

	servers[1].Latency = 30 * time.Millisecond
	selected, matched, err = selectBestServer(user, servers)
	if err != nil {
		t.Fatal(err)
	}
	if selected.ID != "1" || matched {
		t.Fatalf("latency should dominate a much slower same-carrier node: id=%s matched=%v", selected.ID, matched)
	}
}

func TestManualServerSelection(t *testing.T) {
	user := &speedtest.User{Isp: "China Telecom"}
	servers := speedtest.Servers{
		{ID: "1", Sponsor: "Independent IDC", Latency: 8 * time.Millisecond},
		{ID: "2", Sponsor: "China Telecom", Latency: 12 * time.Millisecond},
	}
	selected, matched, err := selectRequestedServer(user, servers, "2")
	if err != nil || selected.ID != "2" || !matched {
		t.Fatalf("manual server selection failed: selected=%#v matched=%v err=%v", selected, matched, err)
	}
	if _, _, err = selectRequestedServer(user, servers, "404"); err == nil {
		t.Fatal("missing manual server must return an error")
	}
}

func TestCarrierAliases(t *testing.T) {
	cases := [][2]string{
		{"China Telecom", "中国电信 Shanghai"},
		{"China Unicom", "CUCC Beijing"},
		{"China Mobile", "CMCC Guangdong"},
	}
	for _, tc := range cases {
		if !carrierMatch(tc[0], tc[1]) {
			t.Errorf("expected carrier match for %q and %q", tc[0], tc[1])
		}
	}
	if carrierMatch("Comcast Cable", "China Mobile") {
		t.Fatal("unexpected carrier match")
	}
}

func TestProfiles(t *testing.T) {
	if !profiles["quick"].SavingMode || profiles["quick"].MaxConnections != 1 {
		t.Fatal("quick profile must use saving mode")
	}
	if profiles["standard"].MaxConnections != 4 || profiles["deep"].MaxConnections != 8 {
		t.Fatal("unexpected standard/deep concurrency")
	}
}
