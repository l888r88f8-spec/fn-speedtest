package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type serverHealthEntry struct {
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checkedAt"`
	LatencyMS float64   `json:"latencyMs,omitempty"`
}

const (
	healthSuccessTTL = 7 * 24 * time.Hour
	healthFailureTTL = time.Hour
)

func (m *multiEngine) setHealthFile(path string) {
	m.mu.Lock()
	m.healthFile = path
	if m.health == nil {
		m.health = map[string]serverHealthEntry{}
	}
	m.mu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	loaded := map[string]serverHealthEntry{}
	if json.Unmarshal(data, &loaded) != nil {
		return
	}
	m.mu.Lock()
	m.health = loaded
	m.mu.Unlock()
}

func (m *multiEngine) currentHealth(id string) serverHealthEntry {
	m.mu.RLock()
	entry, ok := m.health[id]
	m.mu.RUnlock()
	if !ok {
		return serverHealthEntry{}
	}
	age := time.Since(entry.CheckedAt)
	if age < 0 {
		age = 0
	}
	if entry.Status == "success" && age <= healthSuccessTTL {
		return entry
	}
	if entry.Status == "failed" && age <= healthFailureTTL {
		return entry
	}
	return serverHealthEntry{}
}

func (m *multiEngine) applyHealth(option *serverOption) {
	entry := m.currentHealth(option.ID)
	option.HealthStatus = entry.Status
	if option.LatencyMS <= 0 && entry.Status == "success" && entry.LatencyMS > 0 {
		option.LatencyMS = round2(entry.LatencyMS)
		option.LatencyMeasured = true
	}
}

func (m *multiEngine) recordHealth(id string, success bool, latencyMS float64) {
	if id == "" {
		return
	}
	status := "failed"
	if success {
		status = "success"
	}
	entry := serverHealthEntry{Status: status, CheckedAt: time.Now(), LatencyMS: round2(latencyMS)}
	m.mu.Lock()
	if m.health == nil {
		m.health = map[string]serverHealthEntry{}
	}
	m.health[id] = entry
	file := m.healthFile
	snapshot, _ := json.MarshalIndent(m.health, "", "  ")
	m.mu.Unlock()
	if file == "" || len(snapshot) == 0 {
		return
	}
	_ = os.MkdirAll(filepath.Dir(file), 0750)
	tmp := file + ".tmp"
	if os.WriteFile(tmp, snapshot, 0640) == nil {
		_ = os.Rename(tmp, file)
	}
}
