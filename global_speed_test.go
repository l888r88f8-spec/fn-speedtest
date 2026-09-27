package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

const testGlobalKey = "12345678-1234-1234-1234-123456789abc"

type globalMock struct {
	opened, closed, uploaded atomic.Int64
	deny, hang               atomic.Bool
}

func mockGlobal(t *testing.T) (*http.Client, httpTarget, *globalMock) {
	t.Helper()
	m := &globalMock{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/speed/dovalid":
			if r.Method == "POST" {
				if r.URL.Query().Get("key") != testGlobalKey {
					t.Error("wrong release key")
				}
				m.closed.Add(1)
				io.WriteString(w, "1")
				return
			}
			m.opened.Add(1)
			if m.deny.Load() {
				http.Error(w, "denied", 403)
				return
			}
			q := r.URL.Query()
			digest := md5.Sum([]byte("model=Android&imei=" + q.Get("imei") + "&stime=" + q.Get("time")))
			if q.Get("token") != hex.EncodeToString(digest[:]) {
				t.Error("invalid protocol checksum")
			}
			io.WriteString(w, "1-"+testGlobalKey)
		case "/speed/File(1G).dl":
			if r.URL.Query().Get("key") != testGlobalKey || r.URL.Query().Get("ckSize") != "" {
				http.Error(w, "bad key", 403)
				return
			}
			if m.hang.Load() {
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(make([]byte, 1<<20))
		case "/speed/doAnalsLoad.do":
			if r.Header.Get("Key") != "1-"+testGlobalKey {
				http.Error(w, "bad key", 403)
				return
			}
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				http.Error(w, "bad multipart", 400)
				return
			}
			part, err := reader.NextPart()
			if err != nil {
				t.Error(err)
				return
			}
			n, err := io.Copy(io.Discard, part)
			if err != nil {
				return
			}
			m.uploaded.Add(n)
			_, err = reader.NextPart()
			if err != io.EOF {
				t.Error("missing multipart ending")
			}
			io.WriteString(w, "ok")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.Client(), httpTarget{Source: httpSource{ID: "http:gs:test", Name: "南京", Sponsor: "中国电信", Province: "江苏", Carrier: "中国电信", Kind: "globalspeed", Page: server.URL}, Global: true, Network: networkIdentity{PublicIP: "114.114.114.114", Carrier: "中国电信", Province: "江苏", CountryCode: "CN"}, PingURL: server.URL + "/speed/", DownloadURL: server.URL + "/speed/File(1G).dl", UploadURL: server.URL + "/speed/doAnalsLoad.do"}, m
}
func TestGlobalDiscoveryAndSelectedRun(t *testing.T) {
	client, target, mock := mockGlobal(t)
	m := newMultiEngine()
	m.client = client
	m.sources = nil
	calls := 0
	m.globalDirectory = func(context.Context, *http.Client, networkIdentity) ([]httpTarget, error) {
		calls++
		return []httpTarget{target}, nil
	}
	m.detect = func(context.Context) (*speedtest.User, networkIdentity) {
		return &speedtest.User{IP: target.Network.PublicIP}, target.Network
	}
	m.directory = func(context.Context, *http.Client, *speedtest.User, networkIdentity) (serverListResponse, error) {
		return serverListResponse{}, errors.New("offline")
	}
	out := m.discoverGlobal(context.Background(), target.Network)
	if len(out.Servers) != 1 || !out.Servers[0].ProvinceMatched || out.Servers[0].Kind != "globalspeed" {
		t.Fatalf("out=%+v", out)
	}
	if mock.opened.Load() != 1 || mock.closed.Load() != 1 || mock.uploaded.Load() != 4096 {
		t.Fatal("probe did not validate and release session")
	}
	if m.targets[target.Source.ID].target.SessionKey != "" {
		t.Fatal("session credential cached")
	}
	result, err := m.Run(context.Background(), profiles["quick"], target.Source.ID, func(string, int, string) {}, func(liveSample) {})
	if err != nil || result.Engine != "全球网测" || result.DownloadMbps <= 0 || result.UploadMbps <= 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if calls != 1 || mock.opened.Load() != 2 || mock.closed.Load() != 2 {
		t.Fatal("test rediscovered nodes or leaked session")
	}
	b, _ := json.Marshal(result)
	if strings.Contains(string(b), testGlobalKey) {
		t.Fatal("credential exposed")
	}
	mock.deny.Store(true)
	out = m.discoverGlobal(context.Background(), target.Network)
	if len(out.Servers) > 0 {
		t.Fatal("denied node listed")
	}
	if out.Diagnostic.Status != "probe_failed" || !strings.Contains(out.Diagnostic.Message, "HTTP 403") {
		t.Fatalf("failure not reported: %+v", out.Diagnostic)
	}
	if _, ok := m.targets[target.Source.ID]; ok {
		t.Fatal("failed target kept in cache")
	}
}
func TestGlobalCatalogueValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"data":[{"node":[
 {"id":"remote","name":"上海","ip":"114.114.114.114","port":8080,"province":31,"isp":2,"type":0},
 {"id":"same","name":"南京","ip":"114.114.115.115","port":8080,"province":32,"isp":1,"type":0},
 {"id":"duplicate","name":"南京","ip":"114.114.115.115","port":8080,"province":32,"isp":1,"type":0},
 {"id":"private","ip":"127.0.0.1","port":80,"type":0},
 {"id":"other","ip":"114.114.114.114","port":80,"type":1},
 {"id":"unknown","ip":"114.114.114.114","port":80},
 {"id":"wrongport","ip":"114.114.114.114","port":65536,"type":0}
 ]}]}`)
	}))
	defer server.Close()
	targets, err := fetchGlobalTargets(context.Background(), server.Client(), server.URL, networkIdentity{CountryCode: "CN", Province: "江苏", Carrier: "中国电信"})
	if err != nil || len(targets) != 2 || targets[0].Source.Province != "江苏" || targets[0].Source.Carrier != "中国电信" {
		t.Fatalf("targets=%+v err=%v", targets, err)
	}
	if !strings.HasPrefix(targets[0].Source.ID, "http:gs:") || len(targets[0].Source.ID) > 32 {
		t.Fatal("invalid selected ID")
	}
}
func TestGlobalCancelReleasesAndRedacts(t *testing.T) {
	client, target, mock := mockGlobal(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runHTTPTest(ctx, client, target, profiles["quick"], func(phase string, _ int, _ string) {
			if phase == "download" {
				cancel()
			}
		}, func(liveSample) {})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation blocked")
	}
	if mock.closed.Load() != 1 {
		t.Fatal("cancelled session not released")
	}
	// Transport errors can contain query credentials. The public error must not.
	failing := http.Client{Transport: roundTripGlobal(func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Get", URL: "http://example/?key=" + testGlobalKey, Err: errors.New("offline")}
	})}
	_, err := runHTTPTest(context.Background(), &failing, target, profiles["quick"], func(string, int, string) {}, func(liveSample) {})
	if err == nil || strings.Contains(err.Error(), testGlobalKey) {
		t.Fatal("credential leaked in public error")
	}
}

type roundTripGlobal func(*http.Request) (*http.Response, error)

func (f roundTripGlobal) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGlobalReportsFailureForAnyEgress(t *testing.T) {
	m := newMultiEngine()
	calls := 0
	m.globalDirectory = func(context.Context, *http.Client, networkIdentity) ([]httpTarget, error) {
		calls++
		return nil, context.DeadlineExceeded
	}
	out := m.discoverGlobal(context.Background(), networkIdentity{CountryCode: "US"})
	if calls != 1 || out.Diagnostic.Status != "directory_error" || !strings.Contains(out.Diagnostic.Message, "连接超时") {
		t.Fatalf("out=%+v calls=%d", out, calls)
	}
	err := &url.Error{Op: "Get", URL: "http://example/?key=" + testGlobalKey, Err: errors.New("private detail")}
	if strings.Contains(globalFailureReason(err), testGlobalKey) || strings.Contains(globalFailureReason(err), "private") {
		t.Fatal("error details leaked")
	}
}
func TestServersAPIKeepsSourceFailure(t *testing.T) {
	app := newApplication(t.TempDir())
	app.discoverer = fakeDiscoverer{response: serverListResponse{Sources: []sourceDiagnostic{{ID: "globalspeed", Status: "directory_error", Message: "目录获取失败：连接超时"}}}, err: errors.New("没有可用节点")}
	req := httptest.NewRequest("GET", "/api/servers", nil)
	w := httptest.NewRecorder()
	app.routes().ServeHTTP(w, req)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "目录获取失败：连接超时") {
		t.Fatal(w.Code, w.Body.String())
	}
}
