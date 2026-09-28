package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const speedtestCNRawCatalog = "https://raw.githubusercontent.com/spiritLHLS/speedtest.cn-CN-ID/main/CN.csv"

var speedtestCNCatalogURLs = []string{
	"https://cdn.spiritlhl.net/" + speedtestCNRawCatalog,
	speedtestCNRawCatalog,
}

type speedtestCNDiscoveryResult struct {
	Servers    []serverOption
	Diagnostic sourceDiagnostic
}

func speedtestCNCarrier(value string) string {
	if carrier := canonicalCarrier(value); carrier != "" {
		return carrier
	}
	normalized := normalizeCarrier(value)
	if strings.Contains(normalized, "华数") || strings.Contains(normalized, "broadband television") {
		return "中国广电"
	}
	return ""
}

func blockedSpeedtestCNNode(fields ...string) bool {
	text := strings.ToLower(strings.Join(fields, " "))
	return strings.Contains(text, "浙江大学") || strings.Contains(text, "zju.edu.cn")
}

func speedtestCNCustomURL(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "/?#@") {
		return "", errors.New("invalid custom server host")
	}
	u, err := url.Parse("http://" + host + "/upload.php")
	if err != nil || u.Hostname() == "" || u.User != nil {
		return "", errors.New("invalid custom server host")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return "", errors.New("private custom server host")
	}
	return u.String(), nil
}

func catalogURL(value, wantPath string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("invalid endpoint")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && (!ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
		return "", errors.New("private endpoint")
	}
	if !strings.HasSuffix(strings.TrimRight(u.Path, "/"), wantPath) {
		return "", errors.New("unexpected endpoint path")
	}
	return u.String(), nil
}

func parseSpeedtestCNCatalog(data []byte) ([]httpTarget, error) {
	reader := csv.NewReader(strings.NewReader(string(data)))
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, err
	}
	columns := make(map[string]int, len(header))
	for index, name := range header {
		columns[strings.ToLower(strings.TrimSpace(name))] = index
	}
	required := []string{"id", "active", "host", "country_code", "province", "city", "operator", "sponsor", "pingurl", "downloadurl", "uploadurl"}
	for _, name := range required {
		if _, ok := columns[name]; !ok {
			return nil, fmt.Errorf("missing %s column", name)
		}
	}
	get := func(record []string, name string) string {
		index := columns[name]
		if index >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[index])
	}
	targets := make([]httpTarget, 0, 64)
	seen := map[string]bool{}
	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
		if active := get(record, "active"); active == "" || active == "0" || !strings.EqualFold(get(record, "country_code"), "CN") {
			continue
		}
		id, host := get(record, "id"), get(record, "host")
		version := get(record, "ver")
		province := normalizeProvince(get(record, "province"))
		city := strings.TrimSuffix(get(record, "city"), "市")
		operator, sponsor := get(record, "operator"), get(record, "sponsor")
		carrier := speedtestCNCarrier(operator + " " + sponsor)
		if id == "" || province == "" || city == "" || carrier == "" || blockedSpeedtestCNNode(host, city, operator, sponsor) {
			continue
		}
		pingURL, pingErr := catalogURL(get(record, "pingurl"), "/hello")
		downloadURL, downloadErr := catalogURL(get(record, "downloadurl"), "/download")
		uploadURL, uploadErr := catalogURL(get(record, "uploadurl"), "/upload")
		if pingErr != nil || downloadErr != nil || uploadErr != nil {
			continue
		}
		ping, _ := url.Parse(pingURL)
		down, _ := url.Parse(downloadURL)
		up, _ := url.Parse(uploadURL)
		if !strings.EqualFold(ping.Host, down.Host) || !strings.EqualFold(ping.Host, up.Host) || seen[strings.ToLower(ping.Host)] {
			continue
		}
		seen[strings.ToLower(ping.Host)] = true
		customURL, customErr := speedtestCNCustomURL(host)
		if customErr != nil {
			continue
		}
		source := httpSource{ID: "http:cn:" + id, Name: city, Sponsor: carrier, Province: province, Carrier: carrier, Kind: "speedtestcn", Page: "https://www.speedtest.cn/"}
		targets = append(targets, httpTarget{Protocol: "speedtestcn", Version: version, Source: source, PingURL: pingURL, DownloadURL: downloadURL, UploadURL: uploadURL, CustomURL: customURL})
	}
	if len(targets) == 0 {
		return nil, errors.New("catalog contains no eligible nodes")
	}
	return targets, nil
}

func fetchSpeedtestCNCatalogURL(ctx context.Context, client *http.Client, address string) ([]httpTarget, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fnOS-Speedtest/"+appVersion)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 4<<20 {
		return nil, errors.New("catalog too large")
	}
	return parseSpeedtestCNCatalog(data)
}

func fetchSpeedtestCNCatalog(ctx context.Context, client *http.Client, _ networkIdentity) ([]httpTarget, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		targets []httpTarget
		err     error
	}
	results := make(chan result, len(speedtestCNCatalogURLs))
	for _, address := range speedtestCNCatalogURLs {
		go func(address string) {
			targets, err := fetchSpeedtestCNCatalogURL(ctx, client, address)
			results <- result{targets: targets, err: err}
		}(address)
	}
	var lastErr error
	for range speedtestCNCatalogURLs {
		select {
		case item := <-results:
			if item.err == nil && len(item.targets) > 0 {
				cancel()
				return item.targets, nil
			}
			lastErr = item.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("catalog unavailable")
	}
	return nil, lastErr
}

func speedtestCNCandidates(targets []httpTarget, _ networkIdentity, limit int) []httpTarget {
	if limit <= 0 || limit > len(targets) {
		limit = len(targets)
	}
	return append([]httpTarget(nil), targets[:limit]...)
}

func speedtestCNClassicDownloadURL(target httpTarget) (string, error) {
	u, err := url.Parse(target.CustomURL)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("invalid classic Speedtest server")
	}
	u.Path = "/speedtest/random1000x1000.jpg"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func speedtestCNClassicLatencyURL(target httpTarget) (string, error) {
	u, err := url.Parse(target.CustomURL)
	if err != nil || u.Hostname() == "" {
		return "", errors.New("invalid classic Speedtest server")
	}
	u.Path = "/speedtest/latency.txt"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func speedtestCNOneShotLatency(ctx context.Context, client *http.Client, address string) (float64, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	req, err := speedtestCNRequest(probeCtx, http.MethodGet, address, nil, 0)
	if err != nil {
		return 0, err
	}
	started := time.Now()
	resp, err := speedtestCNLooseResponse(client, req)
	if err != nil {
		return 0, err
	}
	_, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if readErr != nil {
		return 0, readErr
	}
	return float64(time.Since(started).Microseconds()) / 1000, nil
}

func speedtestCNDiscoveryLatency(ctx context.Context, _ *http.Client, target httpTarget) (float64, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	return speedtestCNTCPLatency(probeCtx, target)
}

func speedtestCNFirstByteProbe(ctx context.Context, client *http.Client, address string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 1300*time.Millisecond)
	defer cancel()
	req, err := speedtestCNRequest(probeCtx, http.MethodGet, address, nil, 0)
	if err != nil {
		return err
	}
	resp, err := speedtestCNLooseResponse(client, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var b [1]byte
	n, readErr := resp.Body.Read(b[:])
	if n > 0 {
		return nil
	}
	if readErr != nil {
		return readErr
	}
	return errors.New("download endpoint returned no data")
}

func speedtestCNQuickDownloadProbe(ctx context.Context, client *http.Client, target httpTarget) error {
	classicURL, classicErr := speedtestCNClassicDownloadURL(target)
	tryClassic := func() error {
		if classicErr != nil {
			return classicErr
		}
		return speedtestCNFirstByteProbe(ctx, client, classicURL)
	}
	tryDirect := func() error {
		return speedtestCNFirstByteProbe(ctx, client, target.DownloadURL)
	}

	// Match the real runner: classic v1 nodes prefer the ECS/speedtest-go layout;
	// v2/cloud nodes prefer the newer Speedtest.cn /download endpoint.
	if target.Version == "1" {
		if err := tryClassic(); err == nil {
			return nil
		}
		return tryDirect()
	}
	if err := tryDirect(); err == nil {
		return nil
	}
	return tryClassic()
}

func (m *multiEngine) discoverSpeedtestCN(ctx context.Context, n networkIdentity) speedtestCNDiscoveryResult {
	out := speedtestCNDiscoveryResult{Servers: []serverOption{}, Diagnostic: sourceDiagnostic{ID: "speedtestcn", Status: "unavailable", Failures: map[string]int{}}}
	if m.cnDirectory == nil {
		out.Diagnostic.Status = "disabled"
		out.Diagnostic.Message = "未启用"
		return out
	}
	targets, err := m.cnDirectory(ctx, m.client, n)
	if err != nil {
		out.Diagnostic.Status = "directory_error"
		out.Diagnostic.Message = "节点目录获取失败：" + globalFailureReason(err)
		return out
	}
	targets = speedtestCNCandidates(targets, n, len(targets))
	out.Diagnostic.Candidates = len(targets)
	out.Diagnostic.Tested = len(targets)

	type probed struct {
		index           int
		target          httpTarget
		latency         float64
		latencyMeasured bool
		err             error
	}
	jobs := make(chan probed, len(targets))
	results := make(chan probed, len(targets))
	for i, target := range targets {
		target.Network = n
		jobs <- probed{index: i, target: target}
	}
	close(jobs)

	workers := min(32, len(targets))
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for item := range jobs {
				item.latency, item.err = speedtestCNDiscoveryLatency(ctx, m.client, item.target)
				item.latencyMeasured = item.err == nil && item.latency > 0
				results <- item
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	ordered := make([]probed, len(targets))
	for item := range results {
		ordered[item.index] = item
	}
	if ctx.Err() != nil {
		out.Diagnostic.Status = "cancelled"
		out.Diagnostic.Message = "请求已取消"
		return out
	}

	measured := 0
	for _, item := range ordered {
		m.mu.Lock()
		m.targets[item.target.Source.ID] = verifiedTarget{target: item.target, expires: time.Now().Add(30 * time.Minute)}
		m.mu.Unlock()
		if item.latencyMeasured {
			measured++
		} else if item.err != nil {
			out.Diagnostic.Failures["延时探测失败（"+globalFailureReason(item.err)+"）"]++
		}
		s := item.target.Source
		out.Servers = append(out.Servers, serverOption{
			ID: s.ID, Name: s.Name, Sponsor: s.Sponsor, Carrier: s.Carrier,
			Country: "中国", Province: s.Province,
			ProvinceMatched: n.CountryCode == "CN" && n.Province != "" && n.Province == s.Province,
			CarrierMatched: n.Carrier != "" && n.Carrier == s.Carrier,
			Mainland: true, Kind: "speedtestcn", Engine: "Speedtest.cn",
			LatencyMS: round2(item.latency), JitterMS: 0, LatencyMeasured: item.latencyMeasured,
		})
	}
	out.Diagnostic.Available = measured
	if len(out.Servers) == 0 {
		out.Diagnostic.Status = "no_candidates"
		out.Diagnostic.Message = "节点目录没有可用候选"
		return out
	}
	out.Diagnostic.Status = "available"
	out.Diagnostic.Message = fmt.Sprintf("已加载 %d 个节点，%d 个延时可测", len(out.Servers), measured)
	return out
}
func speedtestCNMainFailure(failures map[string]int) string {
	type failure struct {
		name  string
		count int
	}
	items := make([]failure, 0, len(failures))
	for name, count := range failures {
		items = append(items, failure{name: name, count: count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].name < items[j].name
	})
	if len(items) == 0 {
		return ""
	}
	return fmt.Sprintf("%s ×%d", items[0].name, items[0].count)
}
