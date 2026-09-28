package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

type networkIdentity struct {
	PublicIP    string `json:"publicIp"`
	ISP         string `json:"isp,omitempty"`
	Carrier     string `json:"carrier,omitempty"`
	CountryCode string `json:"countryCode,omitempty"`
	Province    string `json:"province,omitempty"`
	City        string `json:"city,omitempty"`
	Source      string `json:"source,omitempty"`
}

type geoRecord struct {
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
	IP          string  `json:"ip"`
	Success     bool    `json:"success"`
	CountryCode string  `json:"country_code"`
	Region      string  `json:"region"`
	City        string  `json:"city"`
	Connection  struct {
		ISP string `json:"isp"`
		Org string `json:"org"`
	} `json:"connection"`
}

type geoCacheEntry struct {
	record  geoRecord
	expires time.Time
}
type ipGeoResolver struct {
	mu       sync.Mutex
	cache    map[string]geoCacheEntry
	client   *http.Client
	endpoint string
}

var networkLookup = &ipGeoResolver{client: &http.Client{Timeout: 4 * time.Second}, endpoint: "https://ipwho.is/"}

// Only the IP obtained by the NAS's outbound Speedtest connection is queried.
// Browser addresses and X-Forwarded-For headers are never used.
func (r *ipGeoResolver) Resolve(ctx context.Context, user *speedtest.User) networkIdentity {
	n := networkIdentity{PublicIP: user.IP, ISP: user.Isp, Carrier: canonicalCarrier(user.Isp), Source: "Speedtest.net"}
	ip, err := netip.ParseAddr(user.IP)
	if err != nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return n
	}
	key := ip.Unmap().String()
	// Serialize metadata lookup so concurrent page loads cannot exhaust the API quota.
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return n
	}
	entry, ok := r.cache[key]
	if !ok || !time.Now().Before(entry.expires) {
		lookupCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		record, err := r.fetch(lookupCtx, key)
		cancel()
		ttl := 30 * time.Minute
		if err != nil {
			record = geoRecord{}
			ttl = time.Minute
		}
		if r.cache == nil {
			r.cache = map[string]geoCacheEntry{}
		}
		// A NAS normally has only one or two egress addresses; keep memory bounded.
		if len(r.cache) >= 64 {
			clear(r.cache)
		}
		entry = geoCacheEntry{record: record, expires: time.Now().Add(ttl)}
		r.cache[key] = entry
	}
	g := entry.record
	if !g.Success {
		return n
	}
	n.CountryCode = strings.ToUpper(g.CountryCode)
	n.City = g.City
	if n.CountryCode == "CN" {
		n.Province = normalizeProvince(g.Region)
	}
	// Preserve the ISP seen by the testing service if it already identifies a carrier.
	if n.Carrier == "" {
		n.Carrier = canonicalCarrier(g.Connection.ISP + " " + g.Connection.Org)
	}
	if n.ISP == "" {
		n.ISP = g.Connection.ISP
	}
	n.Source = "ipwho.is"
	return n
}

func (r *ipGeoResolver) fetch(ctx context.Context, ip string) (geoRecord, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint+ip, nil)
	if err != nil {
		return geoRecord{}, err
	}
	req.Header.Set("User-Agent", "fnOS-Speedtest/"+appVersion)
	resp, err := r.client.Do(req)
	if err != nil {
		return geoRecord{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return geoRecord{}, errors.New("IP lookup unavailable")
	}
	var g geoRecord
	if err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&g); err != nil {
		return geoRecord{}, err
	}
	actual, e1 := netip.ParseAddr(g.IP)
	wanted, e2 := netip.ParseAddr(ip)
	if !g.Success || e1 != nil || !actual.IsGlobalUnicast() || actual.IsPrivate() || (ip != "" && (e2 != nil || actual.Unmap() != wanted.Unmap())) {
		return geoRecord{}, errors.New("IP lookup response mismatch")
	}
	return g, nil
}

func canonicalCarrier(s string) string {
	normalized := normalizeCarrier(s)
	groups := []struct {
		name    string
		aliases []string
	}{
		{"中国电信", []string{"china telecom", "chinanet", "ctcc", "电信"}},
		{"中国联通", []string{"china unicom", "china169", "cncgroup", "cucc", "联通"}},
		{"中国移动", []string{"china mobile", "cmnet", "cmcc", "移动", "china tietong", "铁通"}},
		{"中国广电", []string{"china broadnet", "china broadcasting", "广电"}},
		{"教育网", []string{"cernet", "教育网", "中国教育", "china education and research"}},
	}
	for _, g := range groups {
		if containsCarrierAlias(normalized, g.aliases) {
			return g.name
		}
	}
	return ""
}

type provinceEntry struct {
	name    string
	aliases []string
}

// Province names and a conservative set of unambiguous major-city names. Unknown
// cities remain unclassified; distances alone are not treated as province borders.
var provinces = []provinceEntry{
	{"北京", []string{"Beijing", "Peking", "北京市"}},
	{"天津", []string{"Tianjin", "天津市"}},
	{"河北", []string{"Hebei", "河北省", "Shijiazhuang", "石家庄", "Tangshan", "唐山", "Baoding", "保定"}},
	{"山西", []string{"Shanxi", "山西省", "Taiyuan", "太原", "Datong", "大同"}},
	{"内蒙古", []string{"Inner Mongolia", "Nei Mongol", "内蒙古自治区", "Hohhot", "呼和浩特", "Baotou", "包头"}},
	{"辽宁", []string{"Liaoning", "辽宁省", "Shenyang", "沈阳", "Dalian", "大连"}},
	{"吉林", []string{"Jilin", "吉林省", "Changchun", "长春"}},
	{"黑龙江", []string{"Heilongjiang", "黑龙江省", "Harbin", "哈尔滨", "Daqing", "大庆"}},
	{"上海", []string{"Shanghai", "上海市"}},
	{"江苏", []string{"Jiangsu", "江苏省", "Nanjing", "南京", "苏州", "Wuxi", "无锡", "Kunshan", "昆山", "Changzhou", "常州"}},
	{"浙江", []string{"Zhejiang", "浙江省", "Hangzhou", "杭州", "Ningbo", "宁波", "Wenzhou", "温州", "Jiaxing", "嘉兴"}},
	{"安徽", []string{"Anhui", "安徽省", "Hefei", "合肥", "Wuhu", "芜湖"}},
	{"福建", []string{"Fujian", "福建省", "福州", "Xiamen", "厦门", "Quanzhou", "泉州"}},
	{"江西", []string{"Jiangxi", "江西省", "Nanchang", "南昌", "Ganzhou", "赣州"}},
	{"山东", []string{"Shandong", "山东省", "Jinan", "济南", "Qingdao", "青岛", "Yantai", "烟台"}},
	{"河南", []string{"Henan", "河南省", "Zhengzhou", "郑州", "Luoyang", "洛阳"}},
	{"湖北", []string{"Hubei", "湖北省", "Wuhan", "武汉", "Yichang", "宜昌"}},
	{"湖南", []string{"Hunan", "湖南省", "Changsha", "长沙", "Zhuzhou", "株洲"}},
	{"广东", []string{"Guangdong", "广东省", "Guangzhou", "广州", "Shenzhen", "深圳", "Dongguan", "东莞", "Foshan", "佛山", "Zhuhai", "珠海"}},
	{"广西", []string{"Guangxi", "广西壮族自治区", "Nanning", "南宁", "Guilin", "桂林", "Liuzhou", "柳州"}},
	{"海南", []string{"Hainan", "海南省", "Haikou", "海口", "Sanya", "三亚"}},
	{"重庆", []string{"Chongqing", "重庆市"}},
	{"四川", []string{"Sichuan", "四川省", "Chengdu", "成都", "Mianyang", "绵阳"}},
	{"贵州", []string{"Guizhou", "贵州省", "Guiyang", "贵阳", "Zunyi", "遵义"}},
	{"云南", []string{"Yunnan", "云南省", "Kunming", "昆明"}},
	{"西藏", []string{"Tibet", "Xizang", "西藏自治区", "Lhasa", "拉萨"}},
	{"陕西", []string{"Shaanxi", "陕西省", "Xi'an", "Xian", "西安", "Baoji", "宝鸡"}},
	{"甘肃", []string{"Gansu", "甘肃省", "Lanzhou", "兰州"}},
	{"青海", []string{"Qinghai", "青海省", "Xining", "西宁"}},
	{"宁夏", []string{"Ningxia", "宁夏回族自治区", "Yinchuan", "银川"}},
	{"新疆", []string{"Xinjiang", "新疆维吾尔自治区", "Urumqi", "乌鲁木齐"}},
}

func regionKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.NewReplacer(" sheng", "", " shi", "", " zhuangzu zizhiqu", "", " huizu zizhiqu", "", " uygur zizhiqu", "", " uighur zizhiqu", "", " zizhiqu", "", "壮族自治区", "", "回族自治区", "", "维吾尔自治区", "", "自治区", "", "省", "", "市", "", " province", "", " municipality", "", " autonomous region", "", " hui", "", " uyghur", "", " zhuang", "").Replace(s)
}
func normalizeProvince(s string) string {
	key := regionKey(s)
	for _, p := range provinces {
		if key == regionKey(p.name) {
			return p.name
		}
		// Geolocation's region field must be a province, never infer it from a city.
		for _, alias := range p.aliases[:2] {
			if key == regionKey(alias) {
				return p.name
			}
		}
	}
	return ""
}
func nodeProvince(s *speedtest.Server) string {
	if !isMainland(s) {
		return ""
	}
	name := regionKey(s.Name)
	for _, p := range provinces {
		if name == regionKey(p.name) {
			return p.name
		}
		for _, alias := range p.aliases {
			if name == regionKey(alias) {
				return p.name
			}
		}
	}
	return ""
}
func regionMatched(n networkIdentity, s *speedtest.Server) bool {
	return n.CountryCode == "CN" && n.Province != "" && nodeProvince(s) == n.Province
}
func networkServerScore(user *speedtest.User, n networkIdentity, s *speedtest.Server) float64 {
	score := serverScore(user, s)
	if regionMatched(n, s) {
		score *= .9
	}
	return score
}
func networkQueries(n networkIdentity) []string {
	queries := append([]string{}, discoveryQueries...)
	if n.CountryCode != "CN" {
		return queries
	}
	add := func(s string) {
		if s != "" {
			for _, q := range queries {
				if q == s {
					return
				}
			}
			queries = append(queries, s)
		}
	}
	add(n.City)
	for _, p := range provinces {
		if p.name == n.Province {
			add(p.aliases[0])
			add(p.name)
			break
		}
	}
	return queries
}

func selectBestNetworkServer(user *speedtest.User, n networkIdentity, servers speedtest.Servers) (*speedtest.Server, error) {
	var best *speedtest.Server
	for _, s := range servers {
		if s == nil || s.Latency <= 0 || s.Latency == speedtest.PingTimeout {
			continue
		}
		if best == nil || networkServerScore(user, n, s) < networkServerScore(user, n, best) {
			best = s
		}
	}
	if best == nil {
		return nil, errors.New("没有找到可用的测速节点")
	}
	return best, nil
}
