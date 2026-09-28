package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const (
	appVersion = "1.10.13"
	basePath   = "/app/fnos-speedtest"
)

//go:embed web/*
var webFiles embed.FS

type testResult struct {
	Engine         string           `json:"engine,omitempty"`
	Network        *networkIdentity `json:"network,omitempty"`
	ID             string           `json:"id"`
	Timestamp      string           `json:"timestamp"`
	Profile        string           `json:"profile"`
	LatencyMS      float64          `json:"latencyMs"`
	JitterMS       float64          `json:"jitterMs"`
	DownloadMbps   float64          `json:"downloadMbps"`
	UploadMbps     float64          `json:"uploadMbps"`
	ServerLocation string           `json:"serverLocation,omitempty"`
	ServerID       string           `json:"serverId,omitempty"`
	ServerSponsor  string           `json:"serverSponsor,omitempty"`
	ServerCountry  string           `json:"serverCountry,omitempty"`
	DistanceKM     float64          `json:"distanceKm,omitempty"`
	ISP            string           `json:"isp,omitempty"`
	CarrierMatched bool             `json:"carrierMatched,omitempty"`
	PublicIP       string           `json:"publicIp,omitempty"`
	DurationSec    float64          `json:"durationSec"`
}

type testState struct {
	Status           string      `json:"status"`
	Phase            string      `json:"phase"`
	Progress         int         `json:"progress"`
	Message          string      `json:"message,omitempty"`
	LiveDownloadMbps float64     `json:"liveDownloadMbps,omitempty"`
	LiveUploadMbps   float64     `json:"liveUploadMbps,omitempty"`
	LiveLatencyMS    float64     `json:"liveLatencyMs,omitempty"`
	DownloadProgress int         `json:"downloadProgress"`
	UploadProgress   int         `json:"uploadProgress"`
	Result           *testResult `json:"result,omitempty"`
}

type profile struct {
	Name           string
	SavingMode     bool
	MaxConnections int
}

var profiles = map[string]profile{
	"quick":    {"quick", true, 1},
	"standard": {"standard", false, 4},
	"deep":     {"deep", false, 8},
}

type application struct {
	mu          sync.RWMutex
	state       testState
	history     []testResult
	historyFile string
	cancel      context.CancelFunc
	runner      speedRunner
	discoverer  serverDiscoverer
}

func newApplication(dataDir string) *application {
	_ = os.MkdirAll(dataDir, 0750)
	engine := newMultiEngine()
	engine.setHealthFile(filepath.Join(dataDir, "server-health.json"))
	a := &application{
		state:       testState{Status: "idle", Phase: "idle", Progress: 0},
		historyFile: filepath.Join(dataDir, "history.json"),
		runner:      engine,
		discoverer:  engine,
	}
	a.history = make([]testResult, 0)
	a.loadHistory()
	return a
}

func (a *application) loadHistory() {
	b, err := os.ReadFile(a.historyFile)
	if err != nil {
		return
	}
	var items []testResult
	if json.Unmarshal(b, &items) == nil {
		if items == nil {
			items = make([]testResult, 0)
		}
		if len(items) > 50 {
			items = items[:50]
		}
		a.history = items
	}
}

func (a *application) saveHistoryLocked() {
	b, err := json.MarshalIndent(a.history, "", "  ")
	if err != nil {
		return
	}
	tmp := a.historyFile + ".tmp"
	if os.WriteFile(tmp, b, 0640) == nil {
		_ = os.Rename(tmp, a.historyFile)
	}
}

func (a *application) setState(phase string, progress int, message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Status != "running" {
		return
	}
	a.state.Status = "running"
	a.state.Phase = phase
	a.state.Progress = progress
	a.state.Message = message
}

func (a *application) setLive(sample liveSample) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Status != "running" {
		return
	}
	if sample.DownloadMbps != nil {
		a.state.LiveDownloadMbps = round2(*sample.DownloadMbps)
	}
	if sample.UploadMbps != nil {
		a.state.LiveUploadMbps = round2(*sample.UploadMbps)
	}
	if sample.LatencyMS != nil {
		a.state.LiveLatencyMS = round2(*sample.LatencyMS)
	}
	if sample.DownloadPercent != nil {
		a.state.DownloadProgress = clampPercent(*sample.DownloadPercent)
		if a.state.Phase == "download" {
			a.state.Progress = 45 + a.state.DownloadProgress*28/100
		}
	}
	if sample.UploadPercent != nil {
		a.state.UploadProgress = clampPercent(*sample.UploadPercent)
		if a.state.Phase == "upload" {
			a.state.Progress = 73 + a.state.UploadProgress*23/100
		}
	}
}

func (a *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/info", a.infoHandler)
	mux.HandleFunc("GET /api/state", a.stateHandler)
	mux.HandleFunc("GET /api/servers", a.serversHandler)
	mux.HandleFunc("POST /api/test", a.startHandler)
	mux.HandleFunc("POST /api/test/cancel", a.cancelHandler)
	mux.HandleFunc("GET /api/history", a.historyHandler)
	mux.HandleFunc("DELETE /api/history", a.clearHistoryHandler)

	assets, _ := fs.Sub(webFiles, "web")
	mux.Handle("/", http.FileServer(http.FS(assets)))

	h := securityHeaders(mux)
	root := http.NewServeMux()
	root.Handle(basePath+"/", http.StripPrefix(basePath, h))
	root.HandleFunc(basePath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, basePath+"/", http.StatusTemporaryRedirect)
	})
	root.Handle("/", h)
	return root
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'self'")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *application) infoHandler(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name": "牛速", "version": appVersion, "engine": "Speedtest.cn + Speedtest.net + HTTP", "historyLimit": 50,
	})
}

func (a *application) stateHandler(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	writeJSON(w, http.StatusOK, a.state)
}

func (a *application) serversHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	servers, err := a.discoverer.Discover(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "sources": servers.Sources})
		return
	}
	writeJSON(w, http.StatusOK, servers)
}

func (a *application) historyHandler(w http.ResponseWriter, _ *http.Request) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	items := append([]testResult{}, a.history...)
	writeJSON(w, http.StatusOK, items)
}

func (a *application) clearHistoryHandler(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	a.history = make([]testResult, 0)
	a.saveHistoryLocked()
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *application) startHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Profile string `json:"profile"`
		Server  string `json:"serverId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求参数无效"})
		return
	}
	if input.Profile == "" {
		input.Profile = "standard"
	}
	p, ok := profiles[input.Profile]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未知测速模式"})
		return
	}
	if input.Server == "" || len(input.Server) > 32 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "测速节点参数无效"})
		return
	}

	a.mu.Lock()
	if a.state.Status == "running" || a.state.Status == "cancelling" {
		a.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "测速正在进行中"})
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.state = testState{Status: "running", Phase: "preparing", Progress: 2, Message: "正在连接测速节点"}
	a.mu.Unlock()

	go a.runTest(ctx, p, input.Server)
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (a *application) cancelHandler(w http.ResponseWriter, _ *http.Request) {
	a.mu.Lock()
	if a.state.Status != "running" || a.cancel == nil {
		a.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "当前没有正在运行的测速"})
		return
	}
	cancel := a.cancel
	a.state.Status = "cancelling"
	a.state.Phase = "cancelled"
	a.state.Message = "测速已取消，正在停止后台任务"
	a.mu.Unlock()
	cancel()
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": true})
}

func (a *application) runTest(parent context.Context, p profile, serverID string) {
	ctx, cancel := context.WithTimeout(parent, 6*time.Minute)
	defer cancel()
	started := time.Now()

	result, err := a.runner.Run(ctx, p, serverID, a.setState, a.setLive)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		a.finishError(ctx, err)
		return
	}

	a.setState("finishing", 96, "正在整理结果")
	result.ID = strconv.FormatInt(time.Now().UnixNano(), 36)
	result.Timestamp = time.Now().Format(time.RFC3339)
	result.Profile = p.Name
	result.DurationSec = round2(time.Since(started).Seconds())
	a.mu.Lock()
	if a.state.Status != "running" || ctx.Err() != nil {
		a.mu.Unlock()
		a.finishError(ctx, context.Canceled)
		return
	}
	a.history = append([]testResult{result}, a.history...)
	if len(a.history) > 50 {
		a.history = a.history[:50]
	}
	a.saveHistoryLocked()
	a.state = testState{Status: "complete", Phase: "complete", Progress: 100, DownloadProgress: 100, UploadProgress: 100, Result: &result}
	a.cancel = nil
	a.mu.Unlock()
}

func (a *application) finishError(ctx context.Context, err error) {
	message := "测速失败：" + err.Error()
	status := "error"
	phase := "error"
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		message = "测速已取消"
		status = "cancelled"
		phase = "cancelled"
	} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		message = "测速超时，请检查网络连接后重试"
	}
	a.mu.Lock()
	a.state = testState{Status: status, Phase: phase, Progress: 0, Message: message}
	a.cancel = nil
	a.mu.Unlock()
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func clampPercent(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func main() {
	listenAddr := flag.String("listen", "", "TCP listen address for development, for example 127.0.0.1:32892")
	socketPath := flag.String("socket", "", "Unix socket path for fnOS unified gateway")
	dataDir := flag.String("data-dir", "./data", "persistent data directory")
	flag.Parse()

	app := newApplication(*dataDir)
	server := &http.Server{
		Handler:           app.routes(),
		ReadHeaderTimeout: 8 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	var listener net.Listener
	var err error
	if *socketPath != "" {
		_ = os.Remove(*socketPath)
		listener, err = net.Listen("unix", *socketPath)
		if err == nil {
			_ = os.Chmod(*socketPath, 0660)
		}
	} else {
		if *listenAddr == "" {
			*listenAddr = "127.0.0.1:32892"
		}
		listener, err = net.Listen("tcp", *listenAddr)
	}
	if err != nil {
		log.Fatalf("listen failed: %v", err)
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	log.Printf("fnOS Speedtest %s started", appVersion)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
	if *socketPath != "" {
		_ = os.Remove(*socketPath)
	}
}
