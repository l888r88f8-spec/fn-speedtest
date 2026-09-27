package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveSpeedtestDiscovery(t *testing.T) {
	if os.Getenv("SPEEDTEST_INTEGRATION") != "1" {
		t.Skip("set SPEEDTEST_INTEGRATION=1 to test live discovery")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	got, err := (speedtestNetRunner{}).Discover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mainland := 0
	for _, s := range got.Servers {
		if s.Mainland {
			mainland++
		}
	}
	if len(got.Servers) == 0 || got.RecommendedID == "" {
		t.Fatal("no working nodes")
	}
	t.Logf("working candidates=%d mainland=%d", len(got.Servers), mainland)
}
