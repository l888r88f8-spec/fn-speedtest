package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

// Load the current directory, rather than embedding IDs which may be retired.
var discoveryQueries = []string{"", "China", "Telecom", "Unicom", "Mobile", "电信", "联通", "移动"}

func isMainland(s *speedtest.Server) bool {
	if s.CC != "" {
		return strings.EqualFold(s.CC, "CN")
	}
	switch strings.ToLower(strings.TrimSpace(s.Country)) {
	case "china", "中国", "中国大陆", "mainland china":
		return true
	}
	return false
}

func serverScore(user *speedtest.User, s *speedtest.Server) float64 {
	score := float64(s.Latency)/float64(time.Millisecond) + .5*float64(s.Jitter)/float64(time.Millisecond)
	if carrierMatch(user.Isp, s.Sponsor) {
		score *= .82
	}
	return score + math.Min(s.Distance/2000, 2)
}

func serverDistance(user *speedtest.User, s *speedtest.Server) float64 {
	values := []string{user.Lat, user.Lon, s.Lat, s.Lon}
	var coords [4]float64
	for i, v := range values {
		n, err := strconv.ParseFloat(v, 64)
		limit := 180.0
		if i%2 == 0 {
			limit = 90
		}
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || math.Abs(n) > limit {
			return s.Distance
		}
		coords[i] = n * math.Pi / 180
	}
	a := math.Pow(math.Sin((coords[0]-coords[2])/2), 2) + math.Cos(coords[0])*math.Cos(coords[2])*math.Pow(math.Sin((coords[1]-coords[3])/2), 2)
	return 6378.137 * 2 * math.Asin(math.Sqrt(math.Min(1, math.Max(0, a))))
}

func fetchDirectory(ctx context.Context, client *http.Client, endpoint, keyword string, user *speedtest.User) (speedtest.Servers, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("limit", "100")
	if keyword != "" {
		q.Set("search", keyword)
	}
	q.Set("lat", user.Lat)
	q.Set("lon", user.Lon)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
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
		return nil, fmt.Errorf("directory HTTP %d", resp.StatusCode)
	}
	var servers speedtest.Servers
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&servers)
	return servers, err
}

// Keep nearby choices and reserve room for all three mainland carriers.
func discoveryCandidates(user *speedtest.User, lists []speedtest.Servers) speedtest.Servers {
	return discoveryCandidatesForNetwork(user, lists, networkIdentity{})
}

func discoveryCandidatesForNetwork(user *speedtest.User, lists []speedtest.Servers, n networkIdentity) speedtest.Servers {
	seen := map[string]bool{}
	all := speedtest.Servers{}
	for i, list := range lists {
		for _, s := range list {
			if s == nil || s.ID == "" || s.URL == "" || seen[s.ID] || (i > 0 && !isMainland(s)) {
				continue
			}
			seen[s.ID] = true
			s.Distance = serverDistance(user, s)
			all = append(all, s)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Distance < all[j].Distance })
	candidates := speedtest.Servers{}
	picked := map[string]bool{}
	add := func(s *speedtest.Server) {
		if !picked[s.ID] && len(candidates) < 60 {
			picked[s.ID] = true
			candidates = append(candidates, s)
		}
	}
	for i, s := range all {
		if i < 20 {
			add(s)
		}
	}
	localCount := 0
	for _, s := range all {
		if regionMatched(n, s) && localCount < 10 {
			add(s)
			localCount++
		}
	}
	for _, carrier := range []string{"China Telecom", "China Unicom", "China Mobile"} {
		count := 0
		for _, s := range all {
			if isMainland(s) && carrierMatch(carrier, s.Sponsor) && count < 10 {
				add(s)
				count++
			}
		}
	}
	for _, s := range all {
		if isMainland(s) {
			add(s)
		}
	}
	return candidates
}

// Validate latency.txt as well as HTTP status: an error/login page is not a working node.
func probeServer(ctx context.Context, client *http.Client, s *speedtest.Server) error {
	u, err := url.Parse(s.URL)
	if err != nil {
		return err
	}
	u.Path = path.Join(path.Dir(u.Path), "latency.txt")
	u.RawQuery = ""
	samples := make([]float64, 0, 3)
	for i := 0; i < 4; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "fnOS-Speedtest/"+appVersion)
		began := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		elapsed := time.Since(began)
		if readErr != nil || resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "test=test" {
			return errors.New("invalid Speedtest latency response")
		}
		if i > 0 {
			samples = append(samples, float64(elapsed))
		}
	}
	mean := (samples[0] + samples[1] + samples[2]) / 3
	variance := 0.0
	for _, n := range samples {
		variance += (n - mean) * (n - mean)
	}
	s.Latency = time.Duration(mean)
	s.Jitter = time.Duration(math.Sqrt(variance / 3))
	return nil
}

func discoverServers(ctx context.Context, client *http.Client, endpoint string, user *speedtest.User) (serverListResponse, error) {
	var n networkIdentity
	if user == nil {
		infoClient := newSpeedtestClient(profiles["standard"])
		var err error
		user, err = infoClient.FetchUserInfoContext(ctx)
		if err != nil {
			return serverListResponse{}, networkError("无法获取 Speedtest.net 网络信息", err)
		}
		n = networkLookup.Resolve(ctx, user)
	}
	return discoverForNetwork(ctx, client, endpoint, user, n)
}

func discoverForNetwork(ctx context.Context, client *http.Client, endpoint string, user *speedtest.User, n networkIdentity) (serverListResponse, error) {
	copyUser := *user
	user = &copyUser
	if n.Carrier != "" {
		user.Isp = n.Carrier
	}
	queries := networkQueries(n)
	lists := make([]speedtest.Servers, len(queries))
	var wg sync.WaitGroup
	for i, query := range queries {
		wg.Add(1)
		go func(i int, query string) {
			defer wg.Done()
			fetchCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			// Failure of a supplemental query must not discard working nearby nodes.
			list, err := fetchDirectory(fetchCtx, client, endpoint, query, user)
			if err == nil {
				lists[i] = list
			}
		}(i, query)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return serverListResponse{}, ctx.Err()
	}
	candidates := discoveryCandidatesForNetwork(user, lists, n)
	jobs := make(chan *speedtest.Server, len(candidates))
	for _, s := range candidates {
		jobs <- s
	}
	close(jobs)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				s.Latency = speedtest.PingTimeout
				probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				err := probeServer(probeCtx, client, s)
				cancel()
				if err != nil {
					s.Latency = speedtest.PingTimeout
				}
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return serverListResponse{}, ctx.Err()
	}
	recommended, err := selectBestNetworkServer(user, n, candidates)
	if err != nil {
		return serverListResponse{}, err
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return networkServerScore(user, n, candidates[i]) < networkServerScore(user, n, candidates[j])
	})
	result := serverListResponse{Network: n, ISP: user.Isp, PublicIP: user.IP, RecommendedID: recommended.ID, Servers: make([]serverOption, 0, len(candidates))}
	for _, s := range candidates {
		if s.Latency <= 0 || s.Latency == speedtest.PingTimeout {
			continue
		}
		result.Servers = append(result.Servers, serverOption{
			ID: s.ID, Name: s.Name, Country: s.Country, Sponsor: s.Sponsor,
			Carrier:  canonicalCarrier(s.Sponsor),
			Province: nodeProvince(s), ProvinceMatched: regionMatched(n, s),
			DistanceKM: round2(s.Distance), LatencyMS: round2(float64(s.Latency) / float64(time.Millisecond)),
			JitterMS: round2(float64(s.Jitter) / float64(time.Millisecond)), Mainland: isMainland(s),
			CarrierMatched: carrierMatch(user.Isp, s.Sponsor), Recommended: s.ID == recommended.ID,
		})
	}
	return result, nil
}
