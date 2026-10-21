package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 回归：sources.json 里有自定义源记录时，initialize 不得死锁。
// 根因：initialize 全程占着 nativeState 锁，而 loadSourceRecords 经注册表
// 快照又拿同一把锁；2026-10-21 由 91 源的更新/检测记录触发（0.2.23 首启卡 loading）。
func TestInitializeWithCustomSourceRecord(t *testing.T) {
	dir := t.TempDir()
	records := map[string]any{
		"custom:fc08966bbc2905dc": map[string]any{"stage": "检测未通过"},
		"hongguo":                 map[string]any{},
	}
	body, _ := json.Marshal(records)
	if err := os.WriteFile(filepath.Join(dir, "sources.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
	nativeState.Lock()
	previous := nativeState.engine
	nativeState.engine = nil
	nativeState.Unlock()
	defer func() {
		nativeState.Lock()
		nativeState.engine = previous
		nativeState.Unlock()
	}()
	done := make(chan string, 1)
	go func() {
		raw, _ := json.Marshal(map[string]any{"action": "initialize", "directory": dir})
		done <- NativeRequest(string(raw))
	}()
	select {
	case result := <-done:
		var envelope map[string]any
		if err := json.Unmarshal([]byte(result), &envelope); err != nil || envelope["ok"] != true {
			t.Fatalf("initialize failed: %s", result)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("initialize did not return in 30s (startup deadlock)")
	}
}
