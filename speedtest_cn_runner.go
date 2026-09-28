package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

// runSpeedtestCNHybrid follows the protocol split visible in spiritLHLS/ecs and
// speedtest.cn's catalog.  Version 1 nodes are classic Speedtest servers and are
// best handled by showwin/speedtest-go CustomServer. Version 2 nodes expose the
// newer /hello,/download,/upload API. Whichever generation is primary, the
// other engine remains available as a fallback because public node metadata is
// not always accurate.
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

func newSpeedtestCNCustomClient(p profile) *speedtest.Speedtest {
	return speedtest.New(speedtest.WithUserConfig(&speedtest.UserConfig{
		UserAgent:      speedtestCNBrowserUA,
		PingMode:       speedtest.HTTP,
		SavingMode:     p.SavingMode,
		MaxConnections: p.MaxConnections,
	}))
}

// runSpeedtestCNCustom mirrors the integrated Speedtest.cn path in spiritLHLS/ecs:
// speedtest-go --custom-url=http://HOST/upload.php.  Using the library directly
// keeps cancellation and live progress inside fnOS while preserving the same
// classic latency.txt/random*.jpg/upload.php protocol.
func runSpeedtestCNCustom(ctx context.Context, target httpTarget, p profile, progress progressFunc, sample sampleFunc) (testResult, error) {
	if target.CustomURL == "" {
		return testResult{}, errors.New("ECS兼容模式缺少节点地址")
	}
	client := newSpeedtestCNCustomClient(p)
	var downloadStarted, uploadStarted time.Time
	client.SetCallbackDownload(func(rate speedtest.ByteRate) {
		if sample == nil {
			return
		}
		value := rate.Mbps()
		if value > 0 {
			percent := phasePercent(downloadStarted)
			sample(liveSample{DownloadMbps: &value, DownloadPercent: &percent})
		}
	})
	client.SetCallbackUpload(func(rate speedtest.ByteRate) {
		if sample == nil {
			return
		}
		value := rate.Mbps()
		if value > 0 {
			percent := phasePercent(uploadStarted)
			sample(liveSample{UploadMbps: &value, UploadPercent: &percent})
		}
	})

	progress("connecting", 18, "正在使用 ECS 兼容内核连接 Speedtest.cn 节点")
	server, err := client.CustomServer(target.CustomURL)
	if err != nil {
		return testResult{}, fmt.Errorf("ECS兼容模式节点初始化失败：%w", err)
	}

	// Latency is useful but not a hard gate. Some classic servers disable
	// latency.txt while their random image/upload endpoints still work.
	progress("latency", 31, "正在检测 ECS 兼容节点延迟")
	latencyCtx, cancelLatency := context.WithTimeout(ctx, 8*time.Second)
	pingErr := server.PingTestContext(latencyCtx, func(latency time.Duration) {
		if sample != nil {
			value := float64(latency.Microseconds()) / 1000
			sample(liveSample{LatencyMS: &value})
		}
	})
	cancelLatency()
	if err := ctx.Err(); err != nil {
		return testResult{}, err
	}
	if pingErr != nil {
		// Keep zero latency and continue, matching the app's tolerant CN path.
		server.Latency = 0
		server.Jitter = 0
	}

	progress("download", 45, "正在使用 speedtest-go 测量下载速度")
	downloadPercent := 0
	if sample != nil {
		sample(liveSample{DownloadPercent: &downloadPercent})
	}
	downloadStarted = time.Now()
	downloadCtx, cancelDownload := context.WithTimeout(ctx, 45*time.Second)
	err = server.DownloadTestContext(downloadCtx)
	cancelDownload()
	if ctx.Err() != nil {
		return testResult{}, ctx.Err()
	}
	if err != nil {
		return testResult{}, fmt.Errorf("ECS兼容模式下载失败：%w", err)
	}
	download := server.DLSpeed.Mbps()
	if download <= 0 {
		return testResult{}, errors.New("ECS兼容模式下载未返回有效速度")
	}
	downloadPercent = 100
	if sample != nil {
		sample(liveSample{DownloadMbps: &download, DownloadPercent: &downloadPercent})
	}

	progress("upload", 73, "正在使用 speedtest-go 测量上传速度")
	uploadPercent := 0
	if sample != nil {
		sample(liveSample{UploadPercent: &uploadPercent})
	}
	uploadStarted = time.Now()
	uploadCtx, cancelUpload := context.WithTimeout(ctx, 45*time.Second)
	err = server.UploadTestContext(uploadCtx)
	cancelUpload()
	if ctx.Err() != nil {
		return testResult{}, ctx.Err()
	}
	if err != nil {
		return testResult{}, fmt.Errorf("ECS兼容模式上传失败：%w", err)
	}
	upload := server.ULSpeed.Mbps()
	if upload <= 0 {
		return testResult{}, errors.New("ECS兼容模式上传未返回有效速度")
	}
	uploadPercent = 100
	if sample != nil {
		sample(liveSample{UploadMbps: &upload, UploadPercent: &uploadPercent})
	}

	location := target.Source.Name
	if target.Source.Province != "" {
		location = target.Source.Province + " · " + location
	}
	isp := target.Network.ISP
	if isp == "" {
		isp = target.Network.Carrier
	}
	return testResult{
		Engine:         "Speedtest.cn",
		Network:        &target.Network,
		LatencyMS:      round2(float64(server.Latency.Microseconds()) / 1000),
		JitterMS:       round2(float64(server.Jitter.Microseconds()) / 1000),
		DownloadMbps:   round2(download),
		UploadMbps:     round2(upload),
		ServerLocation: location,
		ServerID:       target.Source.ID,
		ServerSponsor:  target.Source.Sponsor,
		ServerCountry:  "中国",
		ISP:            isp,
		CarrierMatched: target.Network.Carrier != "" && target.Network.Carrier == target.Source.Carrier,
		PublicIP:       target.Network.PublicIP,
	}, nil
}
