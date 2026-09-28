package main

import (
	"path/filepath"
	"testing"
)

func TestServerHealthPersistsAndApplies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server-health.json")
	first := newMultiEngine()
	first.setHealthFile(path)
	first.recordHealth("node-ok", true, 12.5)
	first.recordHealth("node-bad", false, 4.2)

	second := newMultiEngine()
	second.setHealthFile(path)
	ok := serverOption{ID: "node-ok"}
	bad := serverOption{ID: "node-bad"}
	second.applyHealth(&ok)
	second.applyHealth(&bad)
	if ok.HealthStatus != "success" || ok.LatencyMS != 12.5 {
		t.Fatalf("success health=%+v", ok)
	}
	if bad.HealthStatus != "failed" {
		t.Fatalf("failed health=%+v", bad)
	}
}
