package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/showwin/speedtest-go/speedtest"
)

type progressFunc func(phase string, progress int, message string)

type liveSample struct {
	DownloadMbps    *float64
	UploadMbps      *float64
	LatencyMS       *float64
	DownloadPercent *int
	UploadPercent   *int
}

type sampleFunc func(sample liveSample)

type serverOption struct {
	Kind            string  `json:"kind,omitempty"`
	Engine          string  `json:"engine,omitempty"`
	Province        string  `json:"province,omitempty"`
	ProvinceMatched bool    `json:"provinceMatched,omitempty"`
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Country         string  `json:"country,omitempty"`
	Sponsor         string  `json:"sponsor"`
	Carrier         string  `json:"carrier,omitempty"`
	DistanceKM      float64 `json:"distanceKm"`
	LatencyMS       float64 `json:"latencyMs"`
	JitterMS        float64 `json:"jitterMs"`
	LatencyMeasured bool    `json:"latencyMeasured,omitempty"`
	Mainland        bool    `json:"mainland"`
	CarrierMatched  bool    `json:"carrierMatched,omitempty"`
	Recommended     bool    `json:"recommended,omitempty"`
}

type serverListResponse struct {
	Sources       []sourceDiagnostic `json:"sources,omitempty"`
	Network       networkIdentity    `json:"network"`
	ISP           string             `json:"isp,omitempty"`
	PublicIP      string             `json:"publicIp,omitempty"`
	RecommendedID string             `json:"recommendedId"`
	Servers       []serverOption     `json:"servers"`
}

type speedRunner interface {
	Run(ctx context.Context, p profile, serverID string, progress progressFunc, sample sampleFunc) (testResult, error)
}

type serverDiscoverer interface {
	Discover(ctx context.Context) (serverListResponse, error)
}

type speedtestNetRunner struct{}

func newSpeedtestClient(p profile) *speedtest.Speedtest {
	return speedtest.New(speedtest.WithUserConfig(&speedtest.UserConfig{
		UserAgent:      "fnOS-Speedtest/" + appVersion,
		PingMode:       speedtest.HTTP,
		SavingMode:     p.SavingMode,
		MaxConnections: p.MaxConnections,
	}))
}

func (speedtestNetRunner) Discover(ctx context.Context) (serverListResponse, error) {
	return discoverServers(ctx, &http.Client{Timeout: 8 * time.Second}, "https://www.speedtest.net/api/js/servers", nil)
}

func (speedtestNetRunner) Run(ctx context.Context, p profile, serverID string, progress progressFunc, sample sampleFunc) (testResult, error) {
	if serverID == "" {
		return testResult{}, errors.New("请先加载并选择测速节点")
	}
	client := newSpeedtestClient(p)
	var downloadStarted time.Time
	var uploadStarted time.Time
	client.SetCallbackDownload(func(rate speedtest.ByteRate) {
		value := rate.Mbps()
		if value > 0 {
			percent := phasePercent(downloadStarted)
			sample(liveSample{DownloadMbps: &value, DownloadPercent: &percent})
		}
	})
	client.SetCallbackUpload(func(rate speedtest.ByteRate) {
		value := rate.Mbps()
		if value > 0 {
			percent := phasePercent(uploadStarted)
			sample(liveSample{UploadMbps: &value, UploadPercent: &percent})
		}
	})

	progress("detecting", 6, "正在识别公网 IP 和网络运营商")
	user, err := client.FetchUserInfoContext(ctx)
	if err != nil {
		return testResult{}, networkError("无法获取 Speedtest.net 网络信息", err)
	}

	network := networkLookup.Resolve(ctx, user)
	progress("connecting", 16, "正在连接已选择的测速节点")
	server, err := client.FetchServerByIDContext(ctx, serverID)
	if err != nil {
		return testResult{}, networkError("选择的测速节点当前不可用，请刷新附近节点后重试", err)
	}
	matched := carrierMatch(user.Isp, server.Sponsor)

	progress("latency", 31, fmt.Sprintf("正在检测节点延迟：%s", server.Sponsor))
	if err = server.PingTestContext(ctx, nil); err != nil {
		return testResult{}, networkError("所选测速节点延迟检测失败", err)
	}
	latency := float64(server.Latency.Microseconds()) / 1000
	sample(liveSample{LatencyMS: &latency})

	progress("download", 45, fmt.Sprintf("正在从 %s 测量下载速度", server.Name))
	downloadPercent := 0
	sample(liveSample{DownloadPercent: &downloadPercent})
	downloadStarted = time.Now()
	if err = server.DownloadTestContext(ctx); err != nil {
		return testResult{}, networkError("下载测速失败", err)
	}
	if err = ctx.Err(); err != nil {
		return testResult{}, err
	}
	if server.DLSpeed.Mbps() <= 0 {
		return testResult{}, errors.New("下载测速未返回有效结果")
	}
	downloadValue, downloadPercent := server.DLSpeed.Mbps(), 100
	sample(liveSample{DownloadMbps: &downloadValue, DownloadPercent: &downloadPercent})

	progress("upload", 73, fmt.Sprintf("正在向 %s 测量上传速度", server.Name))
	uploadPercent := 0
	sample(liveSample{UploadPercent: &uploadPercent})
	uploadStarted = time.Now()
	if err = server.UploadTestContext(ctx); err != nil {
		return testResult{}, networkError("上传测速失败", err)
	}
	if err = ctx.Err(); err != nil {
		return testResult{}, err
	}
	if server.ULSpeed.Mbps() <= 0 {
		return testResult{}, errors.New("上传测速未返回有效结果")
	}
	uploadValue, uploadPercent := server.ULSpeed.Mbps(), 100
	sample(liveSample{UploadMbps: &uploadValue, UploadPercent: &uploadPercent})

	location := server.Name
	if server.Country != "" {
		location += "（" + server.Country + "）"
	}
	return testResult{
		Engine:         "Speedtest.net",
		Network:        &network,
		LatencyMS:      round2(float64(server.Latency.Microseconds()) / 1000),
		JitterMS:       round2(float64(server.Jitter.Microseconds()) / 1000),
		DownloadMbps:   round2(server.DLSpeed.Mbps()),
		UploadMbps:     round2(server.ULSpeed.Mbps()),
		ServerLocation: location,
		ServerID:       server.ID,
		ServerSponsor:  server.Sponsor,
		ServerCountry:  server.Country,
		DistanceKM:     round2(server.Distance),
		ISP:            user.Isp,
		CarrierMatched: matched,
		PublicIP:       user.IP,
	}, nil
}

func phasePercent(started time.Time) int {
	if started.IsZero() {
		return 0
	}
	percent := int(time.Since(started) * 100 / (15 * time.Second))
	if percent < 1 {
		return 1
	}
	if percent > 99 {
		return 99
	}
	return percent
}

func selectRequestedServer(user *speedtest.User, servers speedtest.Servers, serverID string) (*speedtest.Server, bool, error) {
	if serverID == "" {
		return selectBestServer(user, servers)
	}
	for _, server := range servers {
		if server != nil && server.ID == serverID && server.Latency > 0 && server.Latency != speedtest.PingTimeout {
			return server, carrierMatch(user.Isp, server.Sponsor), nil
		}
	}
	return nil, false, errors.New("选择的测速节点当前不可用，请刷新附近节点后重试")
}

func selectBestServer(user *speedtest.User, servers speedtest.Servers) (*speedtest.Server, bool, error) {
	if len(servers) == 0 {
		return nil, false, errors.New("没有找到可用的 Speedtest.net 测速节点")
	}
	var best *speedtest.Server
	bestScore := math.MaxFloat64
	bestMatched := false
	for _, server := range servers {
		if server == nil || server.Latency <= 0 || server.Latency == speedtest.PingTimeout {
			continue
		}
		matched := carrierMatch(user.Isp, server.Sponsor)
		score := serverScore(user, server)
		if score < bestScore {
			best, bestScore, bestMatched = server, score, matched
		}
	}
	if best == nil {
		return nil, false, errors.New("候选测速节点均不可用")
	}
	return best, bestMatched, nil
}

func carrierMatch(isp, sponsor string) bool {
	if a, b := canonicalCarrier(isp), canonicalCarrier(sponsor); a != "" && b != "" {
		return a == b
	}
	a := normalizeCarrier(isp)
	b := normalizeCarrier(sponsor)
	if a == "" || b == "" {
		return false
	}
	aliasGroups := [][]string{
		{"unicom", "cucc", "联通"},
		{"telecom", "ctcc", "电信"},
		{"mobile", "cmcc", "移动"},
		{"cernet", "教育网"},
	}
	for _, aliases := range aliasGroups {
		if containsCarrierAlias(a, aliases) && containsCarrierAlias(b, aliases) {
			return true
		}
	}
	ignored := map[string]bool{"china": true, "network": true, "internet": true, "communications": true, "communication": true, "group": true, "company": true, "corporation": true, "limited": true}
	for _, word := range strings.Fields(a) {
		if len([]rune(word)) >= 4 && !ignored[word] && strings.Contains(b, word) {
			return true
		}
	}
	return false
}

func containsCarrierAlias(value string, aliases []string) bool {
	for _, alias := range aliases {
		if strings.Contains(value, alias) {
			return true
		}
	}
	return false
}

func normalizeCarrier(value string) string {
	value = strings.ToLower(value)
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsNumber(r))
	}), " ")
}

func networkError(prefix string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s，请检查 NAS 的公网连接", prefix)
}
