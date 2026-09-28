package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

func runSpeedtestCNHybrid(ctx context.Context, httpClient *http.Client, target httpTarget, p profile, progress progressFunc, sample sampleFunc) (testResult, error) {
	type attempt struct {
		name string
		run  func() (testResult, error)
	}
	custom := attempt{name: "ECS/speedtest-go", run: func() (testResult, error) {
		return runSpeedtestCNCustom(ctx, target, p, progress, sample)
	}}
	direct := attempt{name: "Speedtest.cn HTTP", run: func() (testResult, error) {
		return runHTTPTest(ctx, httpClient, target, p, progress, sample)
	}}

	attempts := []attempt{direct, custom}
	if target.Version == "1" {
		attempts = []attempt{custom, direct}
	}
	first, firstErr := attempts[0].run()
	if firstErr == nil {
		return first, nil
	}
	if err := ctx.Err(); err != nil {
		return testResult{}, err
	}
	progress("connecting", 39, "主测速协议不可用，正在自动切换备用协议")
	second, secondErr := attempts[1].run()
	if secondErr == nil {
		return second, nil
	}
	if err := ctx.Err(); err != nil {
		return testResult{}, err
	}
	return testResult{}, fmt.Errorf("Speedtest.cn 两种协议均失败：%s：%v；%s：%v", attempts[0].name, firstErr, attempts[1].name, secondErr)
}

func bundledSpeedtestGoPath() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("FNOS_SPEEDTEST_GO_BIN")); configured != "" {
		info, err := os.Stat(configured)
		if err != nil || !info.Mode().IsRegular() {
			return "", errors.New("指定的 speedtest-go 不存在")
		}
		return configured, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(filepath.Dir(executable), "speedtest-go")
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return "", errors.New("FPK 内置 speedtest-go 不存在或不可执行")
	}
	return candidate, nil
}

type speedtestGoCLIOutput struct {
	Servers []struct {
		Latency time.Duration `json:"latency"`
		Jitter  time.Duration `json:"jitter"`
		DLSpeed float64       `json:"dl_speed"`
		ULSpeed float64       `json:"ul_speed"`
	} `json:"servers"`
}

func runSpeedtestCNCLI(ctx context.Context, target httpTarget, p profile, progress progressFunc, sample sampleFunc) (testResult, error) {
	binary, err := bundledSpeedtestGoPath()
	if err != nil {
		return testResult{}, err
	}
	args := []string{
		"--custom-url=" + target.CustomURL,
		"--json",
		"--ua=" + speedtestCNBrowserUA,
		"--thread=" + strconv.Itoa(max(1, p.MaxConnections)),
	}
	if p.SavingMode {
		args = append(args, "--saving-mode")
	}
	progress("connecting", 18, "正在调用 FPK 内置 speedtest-go v1.8.3")
	progress("download", 45, "正在按 ECS 同款协议执行延迟、下载和上传测速")
	cliCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cliCtx, binary, args...)
	cmd.Env = append(os.Environ(), "NO_COLOR=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, runErr := cmd.Output()
	if cliCtx.Err() != nil {
		if ctx.Err() != nil {
			return testResult{}, ctx.Err()
		}
		return testResult{}, errors.New("内置 speedtest-go 测速超时")
	}
	if runErr != nil {
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 240 {
			detail = detail[:240]
		}
		if detail == "" {
			return testResult{}, fmt.Errorf("内置 speedtest-go 运行失败：%w", runErr)
		}
		return testResult{}, fmt.Errorf("内置 speedtest-go 运行失败：%s", detail)
	}
	var payload speedtestGoCLIOutput
	if err := json.Unmarshal(output, &payload); err != nil || len(payload.Servers) == 0 {
		return testResult{}, errors.New("内置 speedtest-go 返回结果格式异常")
	}
	server := payload.Servers[0]
	download := server.DLSpeed / 125000.0
	upload := server.ULSpeed / 125000.0
	if download <= 0 {
		return testResult{}, errors.New("内置 speedtest-go 下载未返回有效速度")
	}
	if upload <= 0 {
		return testResult{}, errors.New("内置 speedtest-go 上传未返回有效速度")
	}
	latency := float64(server.Latency) / float64(time.Millisecond)
	jitter := float64(server.Jitter) / float64(time.Millisecond)
	if sample != nil {
		dp, up := 100, 100
		sample(liveSample{DownloadMbps: &download, UploadMbps: &upload, LatencyMS: &latency, DownloadPercent: &dp, UploadPercent: &up})
	}
	progress("upload", 90, "内置 speedtest-go 测速完成，正在整理结果")
	return speedtestCNResult(target, latency, jitter, download, upload), nil
}

func runSpeedtestCNCustom(ctx context.Context, target httpTarget, p profile, progress progressFunc, sample sampleFunc) (testResult, error) {
	// The library path exposes live download/upload callbacks, so it is the
	// primary path for the fnOS UI. The bundled CLI remains a compatibility
	// fallback for servers that behave differently under the library path.
	libraryResult, libraryErr := runSpeedtestCNCustomLibrary(ctx, target, p, progress, sample)
	if libraryErr == nil {
		return libraryResult, nil
	}
	if err := ctx.Err(); err != nil {
		return testResult{}, err
	}
	progress("connecting", 24, "实时测速内核未成功，正在使用内置 speedtest-go 兼容模式")
	cliResult, cliErr := runSpeedtestCNCLI(ctx, target, p, progress, sample)
	if cliErr == nil {
		return cliResult, nil
	}
	return testResult{}, fmt.Errorf("实时库：%v；内置CLI：%v", libraryErr, cliErr)
}

func newSpeedtestCNCustomClient(p profile) *speedtest.Speedtest {
	return speedtest.New(speedtest.WithUserConfig(&speedtest.UserConfig{
		UserAgent:      speedtestCNBrowserUA,
		PingMode:       speedtest.HTTP,
		SavingMode:     p.SavingMode,
		MaxConnections: p.MaxConnections,
	}))
}

func runSpeedtestCNCustomLibrary(ctx context.Context, target httpTarget, p profile, progress progressFunc, sample sampleFunc) (testResult, error) {
	if target.CustomURL == "" {
		return testResult{}, errors.New("ECS兼容模式缺少节点地址")
	}
	client := newSpeedtestCNCustomClient(p)
	var downloadStarted, uploadStarted time.Time
	client.SetCallbackDownload(func(rate speedtest.ByteRate) {
		if sample == nil { return }
		value := rate.Mbps()
		if value > 0 {
			percent := phasePercent(downloadStarted)
			sample(liveSample{DownloadMbps: &value, DownloadPercent: &percent})
		}
	})
	client.SetCallbackUpload(func(rate speedtest.ByteRate) {
		if sample == nil { return }
		value := rate.Mbps()
		if value > 0 {
			percent := phasePercent(uploadStarted)
			sample(liveSample{UploadMbps: &value, UploadPercent: &percent})
		}
	})

	progress("connecting", 18, "正在连接实时测速节点")
	server, err := client.CustomServer(target.CustomURL)
	if err != nil {
		return testResult{}, fmt.Errorf("实时引擎节点初始化失败：%w", err)
	}
	progress("latency", 31, "正在实时检测节点延迟")
	latencyCtx, cancelLatency := context.WithTimeout(ctx, 8*time.Second)
	pingErr := server.PingTestContext(latencyCtx, func(latency time.Duration) {
		if sample != nil {
			value := float64(latency.Microseconds()) / 1000
			sample(liveSample{LatencyMS: &value})
		}
	})
	cancelLatency()
	if err := ctx.Err(); err != nil { return testResult{}, err }
	if pingErr != nil { server.Latency = 0; server.Jitter = 0 }

	progress("download", 45, "正在实时测量下载速度")
	downloadPercent := 0
	if sample != nil { sample(liveSample{DownloadPercent: &downloadPercent}) }
	downloadStarted = time.Now()
	downloadCtx, cancelDownload := context.WithTimeout(ctx, 45*time.Second)
	err = server.DownloadTestContext(downloadCtx)
	cancelDownload()
	if ctx.Err() != nil { return testResult{}, ctx.Err() }
	if err != nil { return testResult{}, fmt.Errorf("实时引擎下载失败：%w", err) }
	download := server.DLSpeed.Mbps()
	if download <= 0 { return testResult{}, errors.New("实时引擎下载未返回有效速度") }
	downloadPercent = 100
	if sample != nil { sample(liveSample{DownloadMbps: &download, DownloadPercent: &downloadPercent}) }

	progress("upload", 73, "正在实时测量上传速度")
	uploadPercent := 0
	if sample != nil { sample(liveSample{UploadPercent: &uploadPercent}) }
	uploadStarted = time.Now()
	uploadCtx, cancelUpload := context.WithTimeout(ctx, 45*time.Second)
	err = server.UploadTestContext(uploadCtx)
	cancelUpload()
	if ctx.Err() != nil { return testResult{}, ctx.Err() }
	if err != nil { return testResult{}, fmt.Errorf("实时引擎上传失败：%w", err) }
	upload := server.ULSpeed.Mbps()
	if upload <= 0 { return testResult{}, errors.New("实时引擎上传未返回有效速度") }
	uploadPercent = 100
	if sample != nil { sample(liveSample{UploadMbps: &upload, UploadPercent: &uploadPercent}) }

	latency := float64(server.Latency.Microseconds()) / 1000
	jitter := float64(server.Jitter.Microseconds()) / 1000
	return speedtestCNResult(target, latency, jitter, download, upload), nil
}

func speedtestCNResult(target httpTarget, latency, jitter, download, upload float64) testResult {
	engine := "Speedtest.cn"
	if target.Protocol == "speedtestnet" {
		engine = "Speedtest.net"
	}
	location := target.Source.Name
	if target.Source.Province != "" {
		location = target.Source.Province + " · " + location
	}
	isp := target.Network.ISP
	if isp == "" { isp = target.Network.Carrier }
	return testResult{
		Engine: engine, Network: &target.Network,
		LatencyMS: round2(latency), JitterMS: round2(jitter),
		DownloadMbps: round2(download), UploadMbps: round2(upload),
		ServerLocation: location, ServerID: target.Source.ID,
		ServerSponsor: target.Source.Sponsor, ServerCountry: "中国",
		ISP: isp, CarrierMatched: target.Network.Carrier != "" && target.Network.Carrier == target.Source.Carrier,
		PublicIP: target.Network.PublicIP,
	}
}
