package main

import (
	"context"
	"errors"
	"github.com/showwin/speedtest-go/speedtest"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func mockHTTPSource(t *testing.T) (*httptest.Server, httpTarget, *atomic.Int64) {
	t.Helper()
	var uploaded atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			io.WriteString(w, `<script>const settings={url_dl:'data',url_ul:'empty',url_ping:'empty'};</script>`)
		case "/data":
			if r.Header.Get("Accept-Encoding") != "identity" || r.URL.Query().Get("r") == "" {
				http.Error(w, "missing cache controls", 400)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(make([]byte, 1<<20))
		case "/empty":
			if r.Method == "POST" {
				n, _ := io.Copy(io.Discard, r.Body)
				uploaded.Add(n)
			}
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	source := httpSource{ID: "http:test", Name: "测试高校", Kind: "university", Province: "江苏", Sponsor: "测试大学", Carrier: "教育网", Page: server.URL + "/"}
	target, err := resolveHTTPSource(context.Background(), server.Client(), source)
	if err != nil {
		t.Fatal(err)
	}
	target.Network = networkIdentity{PublicIP: "114.114.114.114", Carrier: "中国电信", Province: "江苏", CountryCode: "CN"}
	return server, target, &uploaded
}
func TestHTTPProbeAndComplete(t *testing.T) {
	server, target, uploaded := mockHTTPSource(t)
	latency, jitter, err := checkHTTPTarget(context.Background(), server.Client(), target, nil)
	if err != nil || latency < 0 || jitter < 0 || uploaded.Load() != 4096 {
		t.Fatalf("probe: %v %v %v upload=%d", latency, jitter, err, uploaded.Load())
	}
	for _, upload := range []bool{false, true} {
		last := 0
		before := uploaded.Load()
		rate, err := measureHTTPPhase(context.Background(), server.Client(), target, httpPhaseConfig{duration: time.Second, connections: 2, budget: 2 << 20, upload: upload}, func(s liveSample) {
			if upload {
				last = *s.UploadPercent
			} else {
				last = *s.DownloadPercent
			}
		})
		if err != nil || rate <= 0 || last != 100 {
			t.Fatalf("phase upload=%v: rate=%v last=%v err=%v", upload, rate, last, err)
		}
		if upload && uploaded.Load()-before != 2<<20 {
			t.Fatalf("upload budget: %d", uploaded.Load()-before)
		}
	}
	// The cached selected ID can run a full quick test with no metadata or discovery calls.
	m := newMultiEngine()
	m.globalDirectory = nil
	m.cnDirectory = nil
	m.client = server.Client()
	m.targets[target.Source.ID] = verifiedTarget{target: target, expires: time.Now().Add(time.Minute)}
	result, err := m.Run(context.Background(), profiles["quick"], target.Source.ID, func(string, int, string) {}, func(liveSample) {})
	if err != nil || result.DownloadMbps <= 0 || result.UploadMbps <= 0 || result.ServerID != target.Source.ID || result.Engine != "HTTP" || result.PublicIP != target.Network.PublicIP {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	m.targets[target.Source.ID] = verifiedTarget{target: target, expires: time.Now().Add(-time.Minute)}
	if _, err = m.Run(context.Background(), profiles["quick"], target.Source.ID, nil, nil); err == nil {
		t.Fatal("expired target accepted")
	}
}
func TestSpeedtestCNRunContinuesWhenLatencyUnavailable(t *testing.T) {
	var uploaded atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(make([]byte, 1<<20))
		case "/upload":
			n, _ := io.Copy(io.Discard, r.Body)
			uploaded.Add(n)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := httpTarget{
		Protocol:    "speedtestcn",
		PingURL:     "http://127.0.0.1:1/hello",
		DownloadURL: server.URL + "/download",
		UploadURL:   server.URL + "/upload",
		Source: httpSource{
			ID: "http:cn:test", Name: "测试节点", Sponsor: "中国电信",
			Province: "江苏", Carrier: "中国电信", Kind: "speedtestcn",
		},
		Network: networkIdentity{PublicIP: "114.114.114.114", Carrier: "中国电信", Province: "江苏", CountryCode: "CN"},
	}

	result, err := runHTTPTest(context.Background(), server.Client(), target, profiles["quick"], func(string, int, string) {}, func(liveSample) {})
	if err != nil {
		t.Fatalf("Speedtest.cn should continue after latency failure: %v", err)
	}
	if result.DownloadMbps <= 0 || result.UploadMbps <= 0 || uploaded.Load() == 0 {
		t.Fatalf("unexpected result=%+v uploaded=%d", result, uploaded.Load())
	}
	if result.LatencyMS != 0 {
		t.Fatalf("failed latency probe should stay unknown/zero, got %v", result.LatencyMS)
	}
}

func TestHTTPPhaseCancellation(t *testing.T) {
	for _, upload := range []bool{false, true} {
		t.Run(map[bool]string{false: "download", true: "upload"}[upload], func(t *testing.T) {
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			samples := atomic.Int64{}
			go func() {
				_, err := measureHTTPPhase(ctx, server.Client(), httpTarget{DownloadURL: server.URL, UploadURL: server.URL}, httpPhaseConfig{duration: time.Minute, connections: 1, budget: 1 << 20, upload: upload}, func(liveSample) { samples.Add(1) })
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("request not started")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel err=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancel did not stop phase")
			}
			if samples.Load() != 0 {
				t.Fatal("cancelled phase emitted progress")
			}
		})
	}
}
func TestHTTPRejectsBrokenEndpoints(t *testing.T) {
	for _, kind := range []string{"html", "redirect", "compressed", "cached", "short", "failed-upload", "login-upload"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					if kind == "failed-upload" {
						http.Error(w, "failed", 503)
						return
					}
					if kind == "login-upload" {
						io.WriteString(w, "<html>Login</html>")
						return
					}
					io.Copy(io.Discard, r.Body)
					return
				}
				if r.URL.Path == "/empty" {
					return
				}
				if kind == "redirect" {
					w.Header().Set("Location", "/empty")
					w.WriteHeader(302)
					return
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				if kind == "html" {
					w.Header().Set("Content-Type", "text/html")
				}
				if kind == "compressed" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				if kind == "cached" {
					w.Header().Set("Age", "20")
				}
				if kind == "short" {
					w.Write([]byte{1})
					return
				}
				w.Write(make([]byte, 1<<20))
			}))
			defer server.Close()
			client := newMultiEngine().client
			target := httpTarget{PingURL: server.URL + "/empty", UploadURL: server.URL + "/empty", DownloadURL: server.URL + "/data"}
			if _, _, err := checkHTTPTarget(context.Background(), client, target, nil); err == nil {
				t.Fatal("invalid target passed")
			}
			if strings.Contains(kind, "upload") {
				emitted := false
				_, err := measureHTTPPhase(context.Background(), client, target, httpPhaseConfig{duration: time.Second, connections: 1, budget: 1 << 20, upload: true}, func(liveSample) { emitted = true })
				if err == nil || emitted {
					t.Fatal("failed upload emitted success")
				}
			}
		})
	}
}
func TestHTTPSourceConfigAndProtection(t *testing.T) {
	for _, kind := range []string{"inline", "defaults", "challenge", "cross-origin", "no-config", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			requests := atomic.Int64{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/speedtest.js" {
					io.WriteString(w, "var Speedtest=function(){};")
					return
				}
				if r.URL.Path == "/speedtest_worker.js" {
					io.WriteString(w, `settings={url_dl:'data',url_ul:'empty',url_ping:'empty'};`)
					return
				}
				switch kind {
				case "inline":
					io.WriteString(w, `<script>s.setParameter("url_dl", "data");s.setParameter("url_ul", "empty");s.setParameter("url_ping", "empty");</script>`)
				case "defaults":
					io.WriteString(w, `<script src="speedtest.js"></script>`)
				case "challenge":
					io.WriteString(w, `Proof of work required <script src="speedtest.js"></script>`)
				case "cross-origin":
					io.WriteString(w, `{url_dl:'https://example.com/data',url_ul:'empty',url_ping:'empty'}`)
				case "no-config":
					io.WriteString(w, `Welcome to speed test`)
				case "redirect":
					w.Header().Set("Location", "/.within.website/")
					w.WriteHeader(302)
				}
			}))
			defer server.Close()
			target, err := resolveHTTPSource(context.Background(), newMultiEngine().client, httpSource{Page: server.URL + "/"})
			success := kind == "inline" || kind == "defaults"
			if success && (err != nil || target.DownloadURL != server.URL+"/data") {
				t.Fatalf("target=%+v err=%v", target, err)
			}
			if !success && err == nil {
				t.Fatal("unverified config accepted")
			}
			if (kind == "challenge" || kind == "redirect") && requests.Load() != 1 {
				t.Fatal("access continued after site protection")
			}
		})
	}
}
func TestPublicSourceMatchAndRanking(t *testing.T) {
	source := publicSources[0]
	for _, n := range []networkIdentity{{CountryCode: "CN", Province: "江苏", Carrier: "中国电信"}, {CountryCode: "CN", Province: "广东", Carrier: "中国联通"}, {}} {
		if sourceEligible(source, n) {
			t.Fatal("restricted carrier source included")
		}
	}
	if !sourceEligible(source, networkIdentity{CountryCode: "CN", Province: "广东", Carrier: "中国电信"}) {
		t.Fatal("matching source excluded")
	}
	list := serverListResponse{Servers: []serverOption{{ID: "other", LatencyMS: 10, Recommended: true}, {ID: "same", LatencyMS: 11, ProvinceMatched: true, CarrierMatched: true}, {ID: "slow", LatencyMS: 90, ProvinceMatched: true, CarrierMatched: true}}}
	rankServerOptions(&list)
	if list.RecommendedID != "same" {
		t.Fatal(list)
	}
	for i, s := range list.Servers {
		if s.Recommended != (i == 0) {
			t.Fatal("multiple recommendations")
		}
	}
}

func TestMultiSourceDiscoveryFallback(t *testing.T) {
	server, target, _ := mockHTTPSource(t)
	m := newMultiEngine()
	m.globalDirectory = nil
	m.cnDirectory = nil
	m.client = server.Client()
	m.sources = []httpSource{target.Source, {ID: "http:broken", Kind: "university", Page: server.URL + "/unavailable"}}
	m.detect = func(context.Context) (*speedtest.User, networkIdentity) {
		return &speedtest.User{IP: target.Network.PublicIP, Isp: "China Telecom"}, target.Network
	}
	m.directory = func(context.Context, *http.Client, *speedtest.User, networkIdentity) (serverListResponse, error) {
		return serverListResponse{}, errors.New("directory offline")
	}
	list, err := m.Discover(context.Background())
	if err != nil || len(list.Servers) != 1 || list.RecommendedID != target.Source.ID || list.Servers[0].Kind != "university" || !list.Servers[0].ProvinceMatched {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if _, ok := m.targets[target.Source.ID]; !ok {
		t.Fatal("verified node was not cached for start")
	}
	if _, ok := m.targets["http:broken"]; ok {
		t.Fatal("invalid node was cached")
	}
}
func TestGeoOwnIPFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"ip":"114.114.114.114","country_code":"CN","region":"Jiangsu Sheng","latitude":32,"longitude":118}`)
	}))
	defer server.Close()
	resolver := ipGeoResolver{endpoint: server.URL + "/", client: server.Client()}
	g, err := resolver.fetch(context.Background(), "")
	if err != nil || g.IP != "114.114.114.114" || g.Latitude != 32 {
		t.Fatalf("own IP=%+v err=%v", g, err)
	}
	if _, err = resolver.fetch(context.Background(), "8.8.8.8"); err == nil {
		t.Fatal("mismatched IP accepted")
	}
}
