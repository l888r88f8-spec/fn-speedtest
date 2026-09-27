package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

// These are candidate public frontends, not assumed-working bandwidth endpoints.
// Only same-origin URLs advertised in a compatible frontend are used. No login,
// verification, signed endpoint or website protection is bypassed.
type httpSource struct {
	ID, Name, Sponsor, Province, Carrier, Kind, Page string
}

var publicSources = []httpSource{
	{"http:gd-telecom", "广东电信", "中国电信", "广东", "中国电信", "operator", "https://10000.gd.cn/html5-speedtest/"},
	{"http:nuaa", "南航虚拟仿真实验平台", "南京航空航天大学", "江苏", "教育网", "university", "https://virtualsim.nuaa.edu.cn/speed/"},
	{"http:sjtu", "上海交通大学", "上海交通大学", "上海", "教育网", "university", "https://wsus.sjtu.edu.cn/speedtest/"},
}

type httpTarget struct {
	Global                          bool
	Protocol                        string
	Host, SessionKey                string
	Source                          httpSource
	Network                         networkIdentity
	PingURL, DownloadURL, UploadURL string
}
type verifiedTarget struct {
	target  httpTarget
	expires time.Time
}
type multiEngine struct {
	globalDirectory func(context.Context, *http.Client, networkIdentity) ([]httpTarget, error)
	cnDirectory     func(context.Context, *http.Client, networkIdentity) ([]httpTarget, error)
	mu              sync.RWMutex
	targets         map[string]verifiedTarget
	sources         []httpSource
	client          *http.Client
	detect          func(context.Context) (*speedtest.User, networkIdentity)
	directory       func(context.Context, *http.Client, *speedtest.User, networkIdentity) (serverListResponse, error)
}

func newMultiEngine() *multiEngine {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableCompression = true
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 16
	transport.ResponseHeaderTimeout = 5 * time.Second
	return &multiEngine{globalDirectory: func(ctx context.Context, client *http.Client, n networkIdentity) ([]httpTarget, error) {
		return fetchGlobalTargets(ctx, client, globalCatalogURL, n)
	}, cnDirectory: fetchSpeedtestCNCatalog, detect: detectNetworkForSources, directory: func(ctx context.Context, client *http.Client, user *speedtest.User, n networkIdentity) (serverListResponse, error) {
		return discoverForNetwork(ctx, client, "https://www.speedtest.net/api/js/servers", user, n)
	}, sources: publicSources, targets: map[string]verifiedTarget{}, client: &http.Client{
		Transport: transport, Timeout: 8 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse },
	}}
}
func sourceEligible(s httpSource, n networkIdentity) bool {
	if s.Kind != "operator" {
		return true
	}
	return n.CountryCode == "CN" && n.Province == s.Province && n.Carrier == s.Carrier
}
func (m *multiEngine) Run(ctx context.Context, p profile, id string, progress progressFunc, sample sampleFunc) (testResult, error) {
	if !strings.HasPrefix(id, "http:") {
		return (speedtestNetRunner{}).Run(ctx, p, id, progress, sample)
	}
	m.mu.RLock()
	cached, ok := m.targets[id]
	m.mu.RUnlock()
	if !ok || time.Now().After(cached.expires) {
		return testResult{}, errors.New("节点验证已过期，请刷新节点后重试")
	}
	// Starting a test uses the selected, validated target; no node discovery is repeated.
	return runHTTPTest(ctx, m.client, cached.target, p, progress, sample)
}

func (m *multiEngine) Discover(ctx context.Context) (serverListResponse, error) {
	user, n := m.detect(ctx)
	type result struct {
		list serverListResponse
		err  error
	}
	speedResults := make(chan result, 1)
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 28*time.Second)
		defer cancel()
		list, err := m.directory(cctx, m.client, user, n)
		speedResults <- result{list, err}
	}()
	cnResults := make(chan speedtestCNDiscoveryResult, 1)
	go func() {
		cctx, cancel := context.WithTimeout(ctx, 28*time.Second)
		defer cancel()
		cnResults <- m.discoverSpeedtestCN(cctx, n)
	}()
	options := make(chan serverOption, len(m.sources))
	var wg sync.WaitGroup
	for _, source := range m.sources {
		if !sourceEligible(source, n) {
			continue
		}
		wg.Add(1)
		go func(source httpSource) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			target, err := resolveHTTPSource(cctx, m.client, source)
			if err != nil {
				m.forget(source.ID)
				return
			}
			target.Network = n
			latency, jitter, err := checkHTTPTarget(cctx, m.client, target, nil)
			if err != nil {
				m.forget(source.ID)
				return
			}
			m.mu.Lock()
			m.targets[source.ID] = verifiedTarget{target: target, expires: time.Now().Add(30 * time.Minute)}
			m.mu.Unlock()
			options <- serverOption{ID: source.ID, Name: source.Name, Sponsor: source.Sponsor, Country: "中国", Province: source.Province,
				Carrier:         source.Carrier,
				ProvinceMatched: n.CountryCode == "CN" && n.Province != "" && n.Province == source.Province,
				CarrierMatched:  n.Carrier != "" && n.Carrier == source.Carrier, Mainland: true, Kind: source.Kind, Engine: "HTTP",
				LatencyMS: round2(latency), JitterMS: round2(jitter)}
		}(source)
	}
	wg.Wait()
	close(options)
	base := <-speedResults
	list := base.list
	list.Network = n
	list.PublicIP = user.IP
	list.ISP = n.Carrier
	if list.ISP == "" {
		list.ISP = user.Isp
	}
	if list.Servers == nil {
		list.Servers = []serverOption{}
	}
	for i := range list.Servers {
		list.Servers[i].Engine = "Speedtest.net"
		list.Servers[i].Kind = "speedtest"
	}
	for option := range options {
		list.Servers = append(list.Servers, option)
	}
	cn := <-cnResults
	list.Servers = append(list.Servers, cn.Servers...)
	list.Sources = []sourceDiagnostic{cn.Diagnostic}
	if ctx.Err() != nil {
		return serverListResponse{}, ctx.Err()
	}
	if len(list.Servers) == 0 {
		return list, errors.New("没有可用测速节点，Speedtest.cn：" + cn.Diagnostic.Message)
	}
	rankServerOptions(&list)
	return list, nil
}
func (m *multiEngine) forget(id string) { m.mu.Lock(); delete(m.targets, id); m.mu.Unlock() }
func rankServerOptions(list *serverListResponse) {
	score := func(s serverOption) float64 {
		if s.Kind == "speedtestcn" && !s.LatencyMeasured {
			// Keep advisory-probe failures selectable, like ecsspeed, but do
			// not recommend them ahead of nodes with a measured latency.
			return 1e9
		}
		v := s.LatencyMS + .5*s.JitterMS
		if s.CarrierMatched {
			v *= .82
		}
		if s.ProvinceMatched {
			v *= .9
		}
		return v
	}
	sort.SliceStable(list.Servers, func(i, j int) bool { return score(list.Servers[i]) < score(list.Servers[j]) })
	list.RecommendedID = list.Servers[0].ID
	for i := range list.Servers {
		list.Servers[i].Recommended = i == 0
	}
}

var scriptPattern = regexp.MustCompile(`(?i)<script[^>]+src\s*=\s*["']([^"']+)["']`)
var parameterPattern = regexp.MustCompile(`(?i)(?:["']?(url_dl|url_ul|url_ping)["']?\s*[:=,]\s*)["']([^"']+)["']`)
var blockedPagePattern = regexp.MustCompile(`(?i)(proof.of.work|pow验证|captcha|\.within\.website|anubis|验证后|统一身份认证|\$_ts)`)

func sameOriginURL(base, reference string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(reference)
	if err != nil {
		return "", err
	}
	u = b.ResolveReference(u)
	if u.Scheme != b.Scheme || !strings.EqualFold(u.Host, b.Host) || u.User != nil || u.Fragment != "" {
		return "", errors.New("测速配置含未支持的跨站地址")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("invalid HTTP scheme")
	}
	if strings.Contains(strings.ToLower(u.Path), "login") || strings.Contains(u.Path, ".within.website") {
		return "", errors.New("网站要求验证")
	}
	return u.String(), nil
}
func readSourceText(ctx context.Context, client *http.Client, address string, limit int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "fnOS-Speedtest/"+appVersion)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("网站访问受限：HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > limit {
		return "", errors.New("网站配置过大")
	}
	text := string(b)
	if blockedPagePattern.MatchString(text) {
		return "", errors.New("网站要求浏览器验证")
	}
	return text, nil
}
func resolveHTTPSource(ctx context.Context, client *http.Client, source httpSource) (httpTarget, error) {
	page, err := readSourceText(ctx, client, source.Page, 1<<20)
	if err != nil {
		return httpTarget{}, err
	}
	// Explicit public configuration is required. We do not guess hidden API paths.
	texts := []string{page}
	workerURL := ""
	scripts := scriptPattern.FindAllStringSubmatch(page, 6)
	for _, match := range scripts {
		if !strings.Contains(strings.ToLower(match[1]), "speedtest") && !strings.Contains(strings.ToLower(match[1]), "app.") {
			continue
		}
		address, err := sameOriginURL(source.Page, match[1])
		if err != nil {
			continue
		}
		text, err := readSourceText(ctx, client, address, 4<<20)
		if err != nil {
			return httpTarget{}, err
		}
		texts = append(texts, text)
		if strings.Contains(strings.ToLower(match[1]), "speedtest.js") {
			workerURL, _ = sameOriginURL(source.Page, "speedtest_worker.js")
		}
	}
	settings := map[string]string{}
	// The frontend's inline overrides take precedence over library defaults.
	for i := len(texts) - 1; i >= 0; i-- {
		for _, match := range parameterPattern.FindAllStringSubmatch(texts[i], -1) {
			settings[strings.ToLower(match[1])] = match[2]
		}
	}
	// LibreSpeed's worker contains the defaults; fetch it only when the frontend
	// explicitly loads the public speedtest.js library.
	if len(settings) < 3 && workerURL != "" {
		worker, err := sameOriginURL(source.Page, workerURL)
		if err != nil {
			return httpTarget{}, err
		}
		text, err := readSourceText(ctx, client, worker, 4<<20)
		if err != nil {
			return httpTarget{}, err
		}
		for _, match := range parameterPattern.FindAllStringSubmatch(text, -1) {
			key := strings.ToLower(match[1])
			if settings[key] == "" {
				settings[key] = match[2]
			}
		}
	}
	target := httpTarget{Source: source}
	for _, item := range []struct {
		key string
		out *string
	}{{"url_dl", &target.DownloadURL}, {"url_ul", &target.UploadURL}, {"url_ping", &target.PingURL}} {
		if settings[item.key] == "" {
			return httpTarget{}, errors.New("未找到公开的兼容测速配置")
		}
		*item.out, err = sameOriginURL(source.Page, settings[item.key])
		if err != nil {
			return httpTarget{}, err
		}
	}
	return target, nil
}

func detectNetworkForSources(ctx context.Context) (*speedtest.User, networkIdentity) {
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	user, err := newSpeedtestClient(profiles["standard"]).FetchUserInfoContext(cctx)
	cancel()
	if err == nil {
		return user, networkLookup.Resolve(ctx, user)
	}
	// Independent HTTP nodes must remain usable when Speedtest's metadata service fails.
	cctx, cancel = context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	g, err := networkLookup.fetch(cctx, "")
	if err != nil {
		return &speedtest.User{}, networkIdentity{}
	}
	user = &speedtest.User{IP: g.IP, Isp: g.Connection.ISP, Lat: fmt.Sprint(g.Latitude), Lon: fmt.Sprint(g.Longitude)}
	n := networkIdentity{PublicIP: g.IP, ISP: g.Connection.ISP, Carrier: canonicalCarrier(g.Connection.ISP + " " + g.Connection.Org), CountryCode: g.CountryCode, City: g.City, Source: "ipwho.is"}
	if n.CountryCode == "CN" {
		n.Province = normalizeProvince(g.Region)
	}
	return user, n
}
