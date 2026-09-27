package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/showwin/speedtest-go/speedtest"
)

func TestDiscoveryDeduplicatesAndFallsBack(t *testing.T) {
	var server *httptest.Server
	var supplementsFail atomic.Bool
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "latency.txt") {
			if strings.HasPrefix(r.URL.Path, "/bad/") {
				http.Error(w, "blocked", http.StatusForbidden)
				return
			}
			fmt.Fprint(w, "test=test\n")
			return
		}
		s := func(id, country, cc, sponsor string) *speedtest.Server {
			return &speedtest.Server{ID: id, Country: country, CC: cc, Name: "Nanjing", Sponsor: sponsor, Lat: "31.23", Lon: "121.47", URL: server.URL + "/" + id + "/upload.php"}
		}
		if supplementsFail.Load() && r.URL.Query().Get("search") != "" {
			http.Error(w, "unavailable", 503)
			return
		}
		switch r.URL.Query().Get("search") {
		case "":
			json.NewEncoder(w).Encode(speedtest.Servers{s("near", "Japan", "JP", "Local ISP")})
		case "China":
			json.NewEncoder(w).Encode(speedtest.Servers{s("cn", "China", "CN", "China Unicom"), s("bad", "China", "CN", "China Telecom"), s("foreign", "Singapore", "SG", "China Telecom")})
		case "Unicom":
			json.NewEncoder(w).Encode(speedtest.Servers{s("cn", "China", "CN", "China Unicom")})
		default:
			http.Error(w, "directory unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	user := &speedtest.User{Isp: "China Unicom", IP: "203.0.113.1", Lat: "31.23", Lon: "121.47"}
	identity := networkIdentity{PublicIP: user.IP, CountryCode: "CN", Province: "江苏", City: "Nanjing", Carrier: "中国联通"}
	got, err := discoverForNetwork(context.Background(), server.Client(), server.URL, user, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Servers) != 2 {
		t.Fatalf("want unique working mainland + nearby, got %#v", got)
	}
	if got.Network.Province != "江苏" {
		t.Fatal("missing detected province in API response")
	}
	recommended := 0
	for _, s := range got.Servers {
		if s.ID != "cn" && s.ID != "near" {
			t.Fatalf("invalid/foreign supplemental server accepted: %#v", s)
		}
		if s.ProvinceMatched != (s.ID == "cn") {
			t.Fatal("incorrect province matching")
		}
		if s.Mainland != (s.ID == "cn") {
			t.Fatal("incorrect mainland classification")
		}
		if s.Recommended {
			recommended++
			if s.ID != got.RecommendedID {
				t.Fatal("recommendation mismatch")
			}
		}
	}
	if recommended != 1 {
		t.Fatal("need exactly one recommendation")
	}
	// Loss of all supplementary feeds must still leave a usable nearby choice.
	supplementsFail.Store(true)
	got, err = discoverServers(context.Background(), server.Client(), server.URL, &speedtest.User{Lat: "31", Lon: "121"})
	if err != nil || len(got.Servers) != 1 || got.Servers[0].ID != "near" {
		t.Fatalf("fallback failed: %v", err)
	}
}

func TestProbeRejectsInvalidResponses(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		ok     bool
	}{{200, "test=test", true}, {200, "<html>login</html>", false}, {503, "test=test", false}} {
		t.Run(fmt.Sprint(tc.status, tc.ok), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); fmt.Fprint(w, tc.body) }))
			defer server.Close()
			s := &speedtest.Server{URL: server.URL + "/speedtest/upload.php"}
			err := probeServer(context.Background(), server.Client(), s)
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v", err)
			}
			if tc.ok && s.Latency <= 0 {
				t.Fatal("missing actual latency")
			}
		})
	}
}

func TestDiscoveryCandidateQuotasAndDistance(t *testing.T) {
	lists := make([]speedtest.Servers, 2)
	for i := 0; i < 100; i++ {
		lists[0] = append(lists[0], &speedtest.Server{ID: fmt.Sprint(i), URL: "http://example.test/upload.php", Country: "Japan", CC: "JP", Distance: float64(i)})
	}
	for i := 0; i < 90; i++ {
		lists[1] = append(lists[1], &speedtest.Server{ID: fmt.Sprint(i + 100), URL: "http://example.test/upload.php", Country: "China", CC: "CN", Distance: 1000 + float64(i), Sponsor: []string{"China Telecom", "China Unicom", "China Mobile"}[i/30]})
	}
	got := discoveryCandidates(&speedtest.User{}, lists)
	if len(got) != 60 {
		t.Fatalf("want bounded 60, got %d", len(got))
	}
	counts := map[string]int{}
	for _, s := range got {
		counts[s.Sponsor]++
	}
	for _, carrier := range []string{"China Telecom", "China Unicom", "China Mobile"} {
		if counts[carrier] < 10 {
			t.Fatalf("lost carrier quota: %v", counts)
		}
	}
	user := &speedtest.User{Lat: "39.9042", Lon: "116.4074"}
	d := serverDistance(user, &speedtest.Server{Lat: "31.2304", Lon: "121.4737"})
	if d < 1060 || d > 1080 {
		t.Fatalf("Beijing-Shanghai distance %f", d)
	}
	if isMainland(&speedtest.Server{CC: "HK", Country: "China"}) {
		t.Fatal("explicit directory code must win")
	}
}

func TestStableServerPreferred(t *testing.T) {
	user := &speedtest.User{}
	servers := speedtest.Servers{{ID: "unstable", Latency: 10 * time.Millisecond, Jitter: 20 * time.Millisecond}, {ID: "stable", Latency: 12 * time.Millisecond, Jitter: time.Millisecond}}
	got, _, err := selectBestServer(user, servers)
	if err != nil || got.ID != "stable" {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestProbeCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := probeServer(ctx, server.Client(), &speedtest.Server{URL: server.URL + "/upload.php"}); err == nil {
		t.Fatal("cancelled probe succeeded")
	}
}
