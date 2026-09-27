package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

func TestNetworkIdentityAndCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/114.114.114.114" {
			t.Errorf("must locate the NAS egress IP: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"success":true,"ip":"114.114.114.114","country_code":"CN","region":"Jiangsu Sheng","city":"Nanjing","connection":{"isp":"China Telecom"}}`)
	}))
	defer server.Close()
	resolver := &ipGeoResolver{client: server.Client(), endpoint: server.URL + "/"}
	user := &speedtest.User{IP: "114.114.114.114", Isp: "CHINANET Jiangsu"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := resolver.Resolve(context.Background(), user)
			if n.Province != "江苏" || n.Carrier != "中国电信" || n.PublicIP != user.IP {
				t.Errorf("unexpected identity: %+v", n)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("expected cached lookup, calls=%d", calls.Load())
	}
	if user.Isp != "CHINANET Jiangsu" {
		t.Fatal("caller data mutated")
	}
}

func TestNetworkLookupFailureAndIPChanges(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"mismatched IP", `{"success":true,"ip":"8.8.8.8","country_code":"CN","region":"Beijing"}`, 200},
		{"rate limit", `{"success":false}`, 429},
		{"invalid JSON", `<html>unavailable</html>`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			resolver := &ipGeoResolver{client: server.Client(), endpoint: server.URL + "/"}
			user := &speedtest.User{IP: "114.114.114.114", Isp: "China Unicom"}
			n := resolver.Resolve(context.Background(), user)
			if n.Province != "" || n.Carrier != "中国联通" || n.Source != "Speedtest.net" {
				t.Fatalf("bad fallback %+v", n)
			}
			resolver.Resolve(context.Background(), user)
			if calls.Load() != 1 {
				t.Fatal("failure must be briefly cached")
			}
			user.IP = "223.5.5.5"
			resolver.Resolve(context.Background(), user)
			if calls.Load() != 2 {
				t.Fatal("changed IP must be re-queried")
			}
		})
	}
}

func TestPrivateIPAndCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	resolver := &ipGeoResolver{client: server.Client(), endpoint: server.URL + "/"}
	for _, ip := range []string{"127.0.0.1", "192.168.1.2", "10.0.0.2", "::1", "fd00::1", "not-an-ip"} {
		resolver.Resolve(context.Background(), &speedtest.User{IP: ip})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolver.Resolve(ctx, &speedtest.User{IP: "114.114.114.114"})
	if calls.Load() != 0 {
		t.Fatal("private addresses and cancelled requests must not make lookups")
	}
}

func TestIPv6Identity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"ip":"2400:3200:0:0:0:0:0:1","country_code":"CN","region":"Zhejiang Sheng","connection":{"isp":"China Telecom"}}`)
	}))
	defer server.Close()
	resolver := &ipGeoResolver{client: server.Client(), endpoint: server.URL + "/"}
	n := resolver.Resolve(context.Background(), &speedtest.User{IP: "2400:3200::1"})
	if n.Province != "浙江" || n.Carrier != "中国电信" {
		t.Fatalf("IPv6 lookup failed %+v", n)
	}
}

func TestProvinceNormalization(t *testing.T) {
	for _, tc := range [][2]string{{"Jiangsu Sheng", "江苏"}, {"Jilin", "吉林"}, {"Guangxi Zhuangzu Zizhiqu", "广西"}, {"Inner Mongolia", "内蒙古"}, {"广西壮族自治区", "广西"}, {"Shanxi", "山西"}, {"Shaanxi Sheng", "陕西"}, {"Nanjing", ""}, {"California", ""}, {"Nei Mongol Zizhiqu", "内蒙古"}} {
		if got := normalizeProvince(tc[0]); got != tc[1] {
			t.Errorf("%s: got %s want %s", tc[0], got, tc[1])
		}
	}
	for _, p := range provinces {
		if normalizeProvince(p.aliases[0]) != p.name {
			t.Errorf("province missing: %s", p.name)
		}
	}
	if nodeProvince(&speedtest.Server{CC: "US", Name: "Nanjing"}) != "" {
		t.Fatal("foreign city must not be a mainland province")
	}
	if nodeProvince(&speedtest.Server{CC: "CN", Name: "Suzhou"}) != "" {
		t.Fatal("ambiguous transliteration must remain unknown")
	}
}

func TestProvincePreferenceKeepsLatencyDominant(t *testing.T) {
	user := &speedtest.User{Isp: "中国电信"}
	n := networkIdentity{CountryCode: "CN", Province: "江苏", City: "Nanjing"}
	servers := speedtest.Servers{
		{ID: "local", CC: "CN", Name: "Nanjing", Sponsor: "CHINANET", Latency: 11 * time.Millisecond},
		{ID: "other", CC: "CN", Name: "Shanghai", Sponsor: "China Telecom", Latency: 10 * time.Millisecond},
	}
	got, err := selectBestNetworkServer(user, n, servers)
	if err != nil || got.ID != "local" {
		t.Fatalf("same-province preference failed: %v %v", got, err)
	}
	servers[0].Latency = 100 * time.Millisecond
	got, err = selectBestNetworkServer(user, n, servers)
	if err != nil || got.ID != "other" {
		t.Fatal("province preference must not force a slow server")
	}
	q := strings.Join(networkQueries(n), "|")
	if !strings.Contains(q, "Jiangsu") || !strings.Contains(q, "Nanjing") {
		t.Fatalf("missing regional discovery: %s", q)
	}
}
