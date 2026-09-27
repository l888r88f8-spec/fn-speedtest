package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/showwin/speedtest-go/speedtest"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Community directory used by taierspeed-cli, not an official public API.
// Only GlobalSpeed entries are accepted; other protocols are not relabelled.
const globalCatalogURL = "https://speed.qwq.vc/api/v1/node"

type globalNode struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IP       string `json:"ip"`
	IPv6     string `json:"ipv6"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	HTTPS    bool   `json:"https"`
	Province int    `json:"province"`
	City     string `json:"city"`
	ISP      int    `json:"isp"`
	Type     *int   `json:"type"`
}

var globalProvinces = map[int]string{11: "北京", 12: "天津", 13: "河北", 14: "山西", 15: "内蒙古", 21: "辽宁", 22: "吉林", 23: "黑龙江", 31: "上海", 32: "江苏", 33: "浙江", 34: "安徽", 35: "福建", 36: "江西", 37: "山东", 41: "河南", 42: "湖北", 43: "湖南", 44: "广东", 45: "广西", 46: "海南", 50: "重庆", 51: "四川", 52: "贵州", 53: "云南", 54: "西藏", 61: "陕西", 62: "甘肃", 63: "青海", 64: "宁夏", 65: "新疆"}
var globalCarriers = map[int]string{1: "中国电信", 2: "中国联通", 3: "中国移动", 4: "教育网", 5: "中国广电"}

func globalTarget(node globalNode, n networkIdentity) (httpTarget, error) {
	if node.Type == nil || *node.Type != 0 {
		return httpTarget{}, errors.New("非全球网测协议")
	}
	ip, err := netip.ParseAddr(node.IP)
	if err != nil {
		ip, err = netip.ParseAddr(node.IPv6)
	}
	if err != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || node.Port < 1 || node.Port > 65535 {
		return httpTarget{}, errors.New("无效节点地址")
	}
	host := node.Host
	if strings.ContainsAny(host, "/\\?#@\r\n\t ") || len(host) > 253 {
		return httpTarget{}, errors.New("无效节点域名")
	}
	scheme := "http"
	if node.HTTPS {
		scheme = "https"
	}
	base := scheme + "://" + net.JoinHostPort(ip.String(), strconv.Itoa(node.Port))
	hash := sha256.Sum256([]byte(base + "|" + host))
	if node.Province >= 70 {
		return httpTarget{}, errors.New("非中国大陆节点")
	}
	province := globalProvinces[node.Province]
	if province == "" {
		province = nodeProvince(&speedtest.Server{Name: node.City, CC: "CN"})
	}
	if province == "" {
		province = nodeProvince(&speedtest.Server{Name: node.Name, CC: "CN"})
	}
	if province == "" {
		return httpTarget{}, errors.New("节点省份无法确认")
	}
	source := httpSource{ID: "http:gs:" + hex.EncodeToString(hash[:8]), Name: node.Name, Sponsor: globalCarriers[node.ISP], Province: province, Carrier: globalCarriers[node.ISP], Kind: "globalspeed", Page: base}
	if source.Name == "" {
		source.Name = node.City
	}
	if source.Name == "" {
		source.Name = "全球网测节点"
	}
	if source.Sponsor == "" {
		source.Sponsor = "全球网测"
	}
	if host != "" && node.Port != 80 && node.Port != 443 {
		host = net.JoinHostPort(host, strconv.Itoa(node.Port))
	}
	return httpTarget{Source: source, Network: n, Global: true, Host: host, PingURL: base + "/speed/", DownloadURL: base + "/speed/File(1G).dl", UploadURL: base + "/speed/doAnalsLoad.do"}, nil
}
func fetchGlobalTargets(ctx context.Context, client *http.Client, address string, n networkIdentity) ([]httpTarget, error) {
	req, err := httpTestRequest(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, globalHTTPFailure(resp.StatusCode)
	}
	var envelope struct {
		Code *int `json:"code"`
		Data []struct {
			Node []globalNode `json:"node"`
		} `json:"data"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) > 4<<20 {
		return nil, errors.New("节点目录过大")
	}
	if err = json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.Code == nil || *envelope.Code != 0 {
		return nil, errors.New("节点目录返回失败")
	}
	targets := []httpTarget{}
	seen := map[string]bool{}
	for _, group := range envelope.Data {
		for _, node := range group.Node {
			target, err := globalTarget(node, n)
			if err != nil || seen[target.Source.ID] {
				continue
			}
			seen[target.Source.ID] = true
			targets = append(targets, target)
		}
	}
	preference := func(t httpTarget) int {
		v := 0
		if n.Carrier != "" && t.Source.Carrier == n.Carrier {
			v += 2
		}
		if n.Province != "" && t.Source.Province == n.Province {
			v += 3
		}
		return v
	}
	sort.SliceStable(targets, func(i, j int) bool { return preference(targets[i]) > preference(targets[j]) })
	if len(targets) > 12 {
		targets = targets[:12]
	}
	return targets, nil
}

var globalKeyPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Protocol compatibility with the existing public CLI: the server decides
// whether to issue a short-lived measurement session. No embedded secret,
// TLS bypass, alternate identity retry, or reuse of another user's key.
func openGlobalSession(ctx context.Context, client *http.Client, target httpTarget) (httpTarget, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return target, err
	}
	clientID := "NS" + hex.EncodeToString(nonce)
	now := strconv.FormatInt(time.Now().Unix(), 10)
	digest := md5.Sum([]byte("model=Android&imei=" + clientID + "&stime=" + now))
	u, _ := url.Parse(target.Source.Page + "/speed/dovalid")
	q := u.Query()
	q.Set("key", "")
	q.Set("flag", "true")
	q.Set("bandwidth", "200")
	q.Set("model", "Android")
	q.Set("imei", clientID)
	q.Set("time", now)
	q.Set("token", hex.EncodeToString(digest[:]))
	u.RawQuery = q.Encode()
	req, err := httpTestRequest(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return target, err
	}
	req.Host = target.Host
	resp, err := client.Do(req)
	if err != nil {
		return target, globalStage("会话连接", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return target, globalStage("会话校验", globalHTTPFailure(resp.StatusCode))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
	if err != nil {
		return target, errors.New("全球网测校验响应读取失败")
	}
	answer := strings.TrimSpace(string(b))
	if !strings.HasPrefix(answer, "1-") || !globalKeyPattern.MatchString(strings.TrimPrefix(answer, "1-")) {
		return target, globalStage("会话校验", globalIssue("节点拒绝、繁忙或协议不匹配"))
	}
	target.SessionKey = strings.TrimPrefix(answer, "1-")
	return target, nil
}
func closeGlobalSession(client *http.Client, target httpTarget) {
	if target.SessionKey == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	u, _ := url.Parse(target.Source.Page + "/speed/dovalid")
	q := u.Query()
	q.Set("key", target.SessionKey)
	u.RawQuery = q.Encode()
	req, err := httpTestRequest(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return
	}
	req.Host = target.Host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
	}
}

// All session material stays in per-test memory and is absent from API/history.
func targetRequest(ctx context.Context, target httpTarget, method, address string, body io.Reader, size int64) (*http.Request, error) {
	if !target.Global {
		if target.Protocol == "speedtestcn" {
			return speedtestCNRequest(ctx, method, address, body, size)
		}
		req, err := httpTestRequest(ctx, method, address, body)
		if err == nil && body != nil {
			req.ContentLength = size
		}
		return req, err
	}
	if target.SessionKey == "" {
		return nil, errors.New("全球网测会话未就绪")
	}
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Del("ckSize")
	if method == http.MethodGet {
		q.Set("key", target.SessionKey)
	}
	u.RawQuery = q.Encode()
	contentLength := size
	if method == http.MethodPost {
		prefix := "--niuspeed-boundary\r\nContent-Disposition: form-data; name=\"upload\"; filename=\"speed.dat\"\r\nContent-Type: application/octet-stream\r\n\r\n"
		suffix := "\r\n--niuspeed-boundary--\r\n"
		body = io.MultiReader(strings.NewReader(prefix), body, strings.NewReader(suffix))
		contentLength += int64(len(prefix) + len(suffix))
	}
	req, err := httpTestRequest(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Host = target.Host
	if method == http.MethodPost {
		req.ContentLength = contentLength
		req.Header.Set("Key", "1-"+target.SessionKey)
		req.Header.Set("Content-Type", "multipart/form-data; boundary=niuspeed-boundary")
	}
	return req, nil
}
func targetUploadResponse(client *http.Client, target httpTarget, req *http.Request) error {
	if target.Protocol == "speedtestcn" {
		resp, err := speedtestCNResponse(client, req, false)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return err
	}
	if !target.Global {
		return emptyHTTPResponse(client, req)
	}
	resp, err := httpTestResponse(client, req, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1025))
	if err != nil {
		return err
	}
	answer := strings.TrimSpace(string(b))
	if answer != "" && answer != "ok" && answer != "OK" && answer != "success" && answer != "1" {
		return globalIssue("上传确认格式不匹配")
	}
	return nil
}
func globalPing(ctx context.Context, client *http.Client, target httpTarget) error {
	req, err := targetRequest(ctx, target, http.MethodGet, target.DownloadURL, nil, 0)
	if err != nil {
		return err
	}
	resp, err := httpTestResponse(client, req, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
	if err != nil || n != 1 {
		return errors.New("全球网测下载探测数据不足")
	}
	return nil
}
func (m *multiEngine) discoverGlobal(ctx context.Context, n networkIdentity) globalDiscoveryResult {
	out := globalDiscoveryResult{Servers: []serverOption{}, Diagnostic: sourceDiagnostic{ID: "globalspeed", Status: "unavailable", Failures: map[string]int{}}}
	if m.globalDirectory == nil {
		out.Diagnostic.Status = "disabled"
		out.Diagnostic.Message = "未启用"
		return out
	}
	// IP geolocation is a preference, not a reason to skip connectivity testing.
	targets, err := m.globalDirectory(ctx, m.client, n)
	if err != nil {
		out.Diagnostic.Status = "directory_error"
		out.Diagnostic.Message = "目录获取失败：" + globalFailureReason(err)
		return out
	}
	out.Diagnostic.Candidates = len(targets)
	if len(targets) == 0 {
		out.Diagnostic.Status = "no_candidates"
		out.Diagnostic.Message = "目录没有符合条件的节点"
		return out
	}
	var lock sync.Mutex
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for _, target := range targets {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(target httpTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			lock.Lock()
			out.Diagnostic.Tested++
			lock.Unlock()
			failed := func(err error) {
				m.forget(target.Source.ID)
				lock.Lock()
				out.Diagnostic.Failures[globalPhase(err)+"失败（"+globalFailureReason(err)+"）"]++
				lock.Unlock()
			}
			probeCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
			defer cancel()
			session, err := openGlobalSession(probeCtx, m.client, target)
			if err != nil {
				failed(err)
				return
			}
			latency, jitter, err := checkHTTPTarget(probeCtx, m.client, session, nil)
			closeGlobalSession(m.client, session)
			if err != nil {
				failed(err)
				return
			}
			if ctx.Err() != nil {
				failed(ctx.Err())
				return
			}
			m.mu.Lock()
			m.targets[target.Source.ID] = verifiedTarget{target: target, expires: time.Now().Add(30 * time.Minute)}
			m.mu.Unlock()
			s := target.Source
			option := serverOption{ID: s.ID, Name: s.Name, Sponsor: s.Sponsor, Carrier: s.Carrier, Country: "中国", Province: s.Province, Mainland: true, Kind: "globalspeed", Engine: "全球网测", LatencyMS: round2(latency), JitterMS: round2(jitter), ProvinceMatched: n.CountryCode == "CN" && n.Province != "" && n.Province == s.Province, CarrierMatched: n.Carrier != "" && n.Carrier == s.Carrier}
			lock.Lock()
			out.Servers = append(out.Servers, option)
			lock.Unlock()
		}(target)
	}
	wg.Wait()
	out.Diagnostic.Available = len(out.Servers)
	if len(out.Servers) > 0 {
		out.Diagnostic.Status = "available"
		out.Diagnostic.Message = fmt.Sprintf("%d 个可用节点", len(out.Servers))
		return out
	}
	parts := []string{}
	for reason, count := range out.Diagnostic.Failures {
		parts = append(parts, fmt.Sprintf("%s %d 个", reason, count))
	}
	sort.Strings(parts)
	if out.Diagnostic.Tested < len(targets) {
		parts = append(parts, fmt.Sprintf("未完成 %d 个", len(targets)-out.Diagnostic.Tested))
	}
	out.Diagnostic.Status = "probe_failed"
	out.Diagnostic.Message = strings.Join(parts, "；")
	return out
}

// bytes is used here to keep the probe upload finite and acknowledged.
func globalProbeUpload(ctx context.Context, client *http.Client, target httpTarget, payload []byte) error {
	body := &uploadReader{reader: bytes.NewReader(payload)}
	req, err := targetRequest(ctx, target, http.MethodPost, target.UploadURL, body, int64(len(payload)))
	if err != nil {
		return err
	}
	if err = targetUploadResponse(client, target, req); err != nil {
		return err
	}
	if body.sent.Load() != int64(len(payload)) {
		return errors.New("全球网测未接收完整探测上传")
	}
	return nil
}
