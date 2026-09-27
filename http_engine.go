package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const speedtestCNBrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/99.0.4844.74 Safari/537.36"

func httpTestRequest(ctx context.Context, method, address string, body io.Reader) (*http.Request, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("r", strconv.FormatInt(time.Now().UnixNano(), 36))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fnOS-Speedtest/"+appVersion)
	req.Header.Set("Cache-Control", "no-cache, no-store")
	req.Header.Set("Accept-Encoding", "identity")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	return req, nil
}

// Speedtest.cn's public HTTP endpoints are designed for its browser client.
// Match ecsspeed-cn's request shape: browser UA, no synthetic query string,
// redirects enabled, and form-style content type for raw upload probes.
func speedtestCNRequest(ctx context.Context, method, address string, body io.Reader, size int64) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, address, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", speedtestCNBrowserUA)
	req.Header.Set("Cache-Control", "no-cache, no-store")
	req.Header.Set("Accept-Encoding", "identity")
	if method == http.MethodPost {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return req, nil
}
func httpTestResponse(client *http.Client, req *http.Request, download bool) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*http.Response, error) { resp.Body.Close(); return nil, err }
	if resp.StatusCode != 200 && (!(!download && resp.StatusCode == 204)) {
		return fail(globalHTTPFailure(resp.StatusCode))
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return fail(errors.New("测速响应被压缩，无法准确计量"))
	}
	if download {
		media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if media != "application/octet-stream" {
			return fail(errors.New("下载接口未返回测速数据"))
		}
		if age, _ := strconv.Atoi(resp.Header.Get("Age")); age > 0 {
			return fail(errors.New("下载接口返回缓存数据"))
		}
	}
	return resp, nil
}

func speedtestCNResponse(client *http.Client, req *http.Request, download bool) (*http.Response, error) {
	redirectClient := *client
	redirectClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("测速接口重定向次数过多")
		}
		if next.URL.Scheme != "http" && next.URL.Scheme != "https" {
			return errors.New("测速接口重定向协议无效")
		}
		next.Header.Set("User-Agent", speedtestCNBrowserUA)
		next.Header.Set("Cache-Control", "no-cache, no-store")
		next.Header.Set("Accept-Encoding", "identity")
		return nil
	}
	resp, err := redirectClient.Do(req)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*http.Response, error) { resp.Body.Close(); return nil, err }
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail(globalHTTPFailure(resp.StatusCode))
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return fail(errors.New("测速响应被压缩，无法准确计量"))
	}
	if download {
		if age, _ := strconv.Atoi(resp.Header.Get("Age")); age > 0 {
			return fail(errors.New("下载接口返回缓存数据"))
		}
	}
	return resp, nil
}

// speedtestCNTCPLatency mirrors ecsspeed's advisory TCP fallback.  It is only
// used when pingUrl HTTP requests cannot produce a usable latency sample.
// A successful TCP handshake proves the endpoint is reachable without
// pretending that the HTTP ping endpoint itself worked.
func speedtestCNTCPLatency(ctx context.Context, target httpTarget) (float64, error) {
	u, err := url.Parse(target.PingURL)
	if err != nil || u.Hostname() == "" {
		return 0, errors.New("测速节点地址无效")
	}
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	started := time.Now()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return 0, err
	}
	_ = conn.Close()
	return float64(time.Since(started).Microseconds()) / 1000, nil
}

func targetDownloadAddress(target httpTarget) string {
	if target.Protocol == "speedtestcn" {
		return target.DownloadURL
	}
	return downloadAddress(target.DownloadURL, 1)
}

func targetDownloadResponse(client *http.Client, target httpTarget, req *http.Request) (*http.Response, error) {
	if target.Protocol == "speedtestcn" {
		return speedtestCNResponse(client, req, true)
	}
	return httpTestResponse(client, req, true)
}

// speedtestCNLatency follows the ecsspeed-cn latency strategy: issue three
// browser-shaped GET requests to pingUrl, follow normal HTTP redirects, accept
// 2xx/3xx responses, and use the lowest successful total request time as the
// displayed latency. Failed attempts do not discard the node as long as one
// request succeeds. Jitter is kept for the fnOS UI and is calculated from the
// successful samples in request order.
func speedtestCNLatency(ctx context.Context, client *http.Client, target httpTarget, sample sampleFunc) (float64, float64, error) {
	redirectClient := *client
	redirectClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("测速接口重定向次数过多")
		}
		if next.URL.Scheme != "http" && next.URL.Scheme != "https" {
			return errors.New("测速接口重定向协议无效")
		}
		next.Header.Set("User-Agent", speedtestCNBrowserUA)
		next.Header.Set("Cache-Control", "no-cache, no-store")
		next.Header.Set("Accept-Encoding", "identity")
		return nil
	}

	values := make([]float64, 0, 3)
	var lastErr error
	for i := 0; i < 3; i++ {
		req, err := speedtestCNRequest(ctx, http.MethodGet, target.PingURL, nil, 0)
		if err != nil {
			return 0, 0, err
		}
		started := time.Now()
		resp, err := redirectClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		statusOK := resp.StatusCode >= 200 && resp.StatusCode < 400
		_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 256<<10))
		resp.Body.Close()
		if !statusOK {
			lastErr = globalHTTPFailure(resp.StatusCode)
			continue
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		value := float64(time.Since(started).Microseconds()) / 1000
		values = append(values, value)
		if sample != nil && ctx.Err() == nil {
			sample(liveSample{LatencyMS: &value})
		}
	}
	if len(values) == 0 {
		if lastErr == nil {
			lastErr = errors.New("没有有效的 HTTP 延迟样本")
		}
		return 0, 0, lastErr
	}

	best := values[0]
	for _, value := range values[1:] {
		if value < best {
			best = value
		}
	}
	if len(values) == 1 {
		return best, 0, nil
	}
	jitter := 0.0
	for i := 1; i < len(values); i++ {
		jitter += math.Abs(values[i] - values[i-1])
	}
	return best, jitter / float64(len(values)-1), nil
}

func speedtestCNPing(ctx context.Context, client *http.Client, target httpTarget) error {
	_, _, err := speedtestCNLatency(ctx, client, target, nil)
	return err
}
func emptyHTTPResponse(client *http.Client, req *http.Request) error {
	resp, err := httpTestResponse(client, req, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
	if err != nil {
		return err
	}
	if len(b) != 0 {
		return errors.New("测速接口未返回空确认响应")
	}
	return nil
}
func downloadAddress(address string, chunks int) string {
	u, _ := url.Parse(address)
	q := u.Query()
	q.Set("ckSize", strconv.Itoa(chunks))
	u.RawQuery = q.Encode()
	return u.String()
}

// Probe only endpoints advertised by an unprotected public frontend. Small
// binary downloads and acknowledged POSTs distinguish real testing endpoints
// from HTML login pages and avoid listing download-only sources as full tests.
func checkHTTPTarget(ctx context.Context, client *http.Client, target httpTarget, sample sampleFunc) (float64, float64, error) {
	var values []float64
	var measuredLatency, measuredJitter float64
	if target.Protocol == "speedtestcn" {
		latencyCtx, latencyCancel := context.WithTimeout(ctx, 8*time.Second)
		measuredLatency, measuredJitter, _ = speedtestCNLatency(latencyCtx, client, target, sample)
		latencyCancel()
		if measuredLatency <= 0 {
			tcpCtx, tcpCancel := context.WithTimeout(ctx, 4*time.Second)
			tcpLatency, tcpErr := speedtestCNTCPLatency(tcpCtx, target)
			tcpCancel()
			if tcpErr == nil {
				measuredLatency = tcpLatency
				measuredJitter = 0
				if sample != nil && ctx.Err() == nil {
					sample(liveSample{LatencyMS: &measuredLatency})
				}
			}
		}
	} else {
		for i := 0; i < 4; i++ {
			req, err := httpTestRequest(ctx, http.MethodGet, target.PingURL, nil)
			if err != nil {
				return 0, 0, globalStage("延迟探测", err)
			}
			started := time.Now()
			if target.Global {
				err = globalPing(ctx, client, target)
			} else {
				err = emptyHTTPResponse(client, req)
			}
			if err != nil {
				return 0, 0, globalStage("延迟探测", err)
			}
			value := float64(time.Since(started).Microseconds()) / 1000
			if i > 0 {
				values = append(values, value)
				if sample != nil && ctx.Err() == nil {
					sample(liveSample{LatencyMS: &value})
				}
			}
		}
	}
	req, err := targetRequest(ctx, target, http.MethodGet, targetDownloadAddress(target), nil, 0)
	if err != nil {
		return 0, 0, globalStage("下载验证", err)
	}
	resp, err := targetDownloadResponse(client, target, req)
	if err != nil {
		return 0, 0, globalStage("下载验证", err)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
	resp.Body.Close()
	if err != nil {
		return 0, 0, globalStage("下载验证", err)
	}
	if n < 32<<10 {
		return 0, 0, globalStage("下载验证", errors.New("下载数据不足"))
	}
	payload := make([]byte, 4096)
	if _, err = rand.Read(payload); err != nil {
		return 0, 0, globalStage("下载验证", err)
	}
	if target.Global {
		if err = globalProbeUpload(ctx, client, target, payload); err != nil {
			return 0, 0, globalStage("上传验证", err)
		}
	} else {
		req, err = targetRequest(ctx, target, http.MethodPost, target.UploadURL, bytes.NewReader(payload), int64(len(payload)))
		if err != nil {
			return 0, 0, globalStage("上传验证", err)
		}
		if err = targetUploadResponse(client, target, req); err != nil {
			return 0, 0, globalStage("上传验证", err)
		}
	}
	if target.Protocol == "speedtestcn" {
		return measuredLatency, measuredJitter, nil
	}
	var latency, jitter float64
	for i, v := range values {
		latency += v
		if i > 0 {
			jitter += math.Abs(v - values[i-1])
		}
	}
	return latency / float64(len(values)), jitter / float64(len(values)-1), nil
}

type uploadReader struct {
	reader *bytes.Reader
	sent   atomic.Int64
}

func (r *uploadReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.sent.Add(int64(n))
	return n, err
}

type httpPhaseConfig struct {
	duration    time.Duration
	connections int
	budget      int64
	upload      bool
}

func httpConfig(p profile, upload bool) httpPhaseConfig {
	c := httpPhaseConfig{duration: 10 * time.Second, connections: min(p.MaxConnections, 4), budget: 256 << 20, upload: upload}
	if c.connections < 1 {
		c.connections = 1
	}
	if p.Name == "quick" {
		c.duration = 5 * time.Second
		c.budget = 32 << 20
	}
	if p.Name == "deep" {
		c.duration = 12 * time.Second
		c.budget = 512 << 20
	}
	if upload {
		c.budget /= 2
	}
	return c
}

// Each phase stops at either its time or traffic budget. Rates are bytes actually
// read (download) or fully acknowledged (upload), divided by elapsed time. No
// bytes from failed POSTs are counted. Workers are joined before returning.
func measureHTTPPhase(ctx context.Context, client *http.Client, target httpTarget, c httpPhaseConfig, sample sampleFunc) (float64, error) {
	phaseCtx, cancel := context.WithTimeout(ctx, c.duration)
	defer cancel()
	started := time.Now()
	var total, allocated atomic.Int64
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	fail := func(err error) {
		if phaseCtx.Err() == nil {
			errOnce.Do(func() { firstErr = err; cancel() })
		}
	}
	for i := 0; i < c.connections; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := make([]byte, 1<<20)
			if c.upload {
				if _, err := rand.Read(payload); err != nil {
					fail(err)
					return
				}
			}
			for phaseCtx.Err() == nil {
				// Fixed chunks keep memory bounded and reserve the total traffic budget.
				offset := allocated.Add(int64(len(payload))) - int64(len(payload))
				if offset >= c.budget {
					return
				}
				size := min(int64(len(payload)), c.budget-offset)
				method, address := http.MethodGet, targetDownloadAddress(target)
				var body io.Reader
				var uploadBody *uploadReader
				if c.upload {
					method, address = http.MethodPost, target.UploadURL
					uploadBody = &uploadReader{reader: bytes.NewReader(payload[:size])}
					body = uploadBody
				}
				req, err := targetRequest(phaseCtx, target, method, address, body, size)
				if err != nil {
					fail(err)
					return
				}
				if c.upload {
					if err = targetUploadResponse(client, target, req); err != nil {
						fail(err)
						return
					}
					if uploadBody.sent.Load() != size {
						fail(errors.New("上传数据未完整发送"))
						return
					}
					total.Add(size)
				} else {
					resp, err := targetDownloadResponse(client, target, req)
					if err != nil {
						fail(err)
						return
					}
					reader := io.LimitReader(resp.Body, size)
					count := int64(0)
					buffer := payload[:32<<10]
					for {
						n, readErr := reader.Read(buffer)
						if n > 0 {
							total.Add(int64(n))
							count += int64(n)
						}
						if readErr != nil {
							if readErr != io.EOF {
								err = readErr
							}
							break
						}
					}
					resp.Body.Close()
					if err != nil {
						fail(err)
						return
					}
					if count != size {
						fail(errors.New("下载数据提前结束"))
						return
					}
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	emit := func(percent int) float64 {
		value := float64(total.Load()) * 8 / time.Since(started).Seconds() / 1e6
		if sample != nil && ctx.Err() == nil {
			if c.upload {
				sample(liveSample{UploadMbps: &value, UploadPercent: &percent})
			} else {
				sample(liveSample{DownloadMbps: &value, DownloadPercent: &percent})
			}
		}
		return value
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return 0, ctx.Err()
		case <-done:
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if firstErr != nil {
				return 0, firstErr
			}
			if total.Load() == 0 {
				return 0, errors.New("节点没有返回有效测速数据")
			}
			return emit(100), nil
		case <-ticker.C:
			if phaseCtx.Err() == nil {
				emit(min(99, int(time.Since(started)*100/c.duration)))
			}
		}
	}
}
func runHTTPTest(ctx context.Context, client *http.Client, target httpTarget, p profile, progress progressFunc, sample sampleFunc) (result testResult, retErr error) {
	defer func() {
		if target.Global && retErr != nil {
			if errors.Is(retErr, context.Canceled) {
				retErr = context.Canceled
			} else if errors.Is(retErr, context.DeadlineExceeded) {
				retErr = context.DeadlineExceeded
			} else {
				retErr = errors.New("全球网测节点当前不可用，请刷新节点重试")
			}
		}
	}()
	if target.Global {
		var err error
		target, err = openGlobalSession(ctx, client, target)
		if err != nil {
			return testResult{}, err
		}
		defer closeGlobalSession(client, target)
	}
	progress("latency", 31, "正在检测所选节点延迟")
	latency, jitter, err := checkHTTPTarget(ctx, client, target, sample)
	if err != nil {
		return testResult{}, networkError("所选 HTTP 节点当前不可用，请刷新节点", err)
	}
	progress("download", 45, "正在测量下载速度")
	down, err := measureHTTPPhase(ctx, client, target, httpConfig(p, false), sample)
	if err != nil {
		return testResult{}, networkError("下载测速失败", err)
	}
	if err = ctx.Err(); err != nil {
		return testResult{}, err
	}
	progress("upload", 73, "正在测量上传速度")
	up, err := measureHTTPPhase(ctx, client, target, httpConfig(p, true), sample)
	if err != nil {
		return testResult{}, networkError("上传测速失败", err)
	}
	if err = ctx.Err(); err != nil {
		return testResult{}, err
	}
	n := target.Network
	engine := "HTTP"
	if target.Global {
		engine = "全球网测"
	} else if target.Protocol == "speedtestcn" {
		engine = "Speedtest.cn"
	}
	location := target.Source.Name
	if target.Source.Province != "" && target.Source.Province != location {
		location = target.Source.Province + " · " + location
	}
	return testResult{Engine: engine, Network: &n, LatencyMS: round2(latency), JitterMS: round2(jitter), DownloadMbps: round2(down), UploadMbps: round2(up), ServerLocation: location + "（中国）", ServerID: target.Source.ID, ServerSponsor: target.Source.Sponsor, ServerCountry: "中国", ISP: n.ISP, PublicIP: n.PublicIP, CarrierMatched: n.Carrier != "" && n.Carrier == target.Source.Carrier}, nil
}
