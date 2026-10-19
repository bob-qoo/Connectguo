package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeBuildAuthorization(t *testing.T) {
	for _, source := range []string{sourceHongguo, sourceHuangdou, sourceHuangguoVideo, sourceHuangguoAI, sourceCloudFront, "unknown"} {
		allowed := source != "unknown" // Connectguo: 全版本开放全部站源
		for _, action := range []string{"catalog", "cached", "sourceStatus", "sourceJob", "cancelSourceJob", "detail", "cover", "resolve", "enqueueDownloads", "localPlayback"} {
			input := nativeInput{Action: action, Source: source, Drama: nativeDrama{ID: source + ":123", Source: source}}
			err := nativeAuthorizeInput(input)
			if (err == nil) != allowed {
				t.Fatalf("%s %s: allowed=%v, error=%v", action, source, allowed, err)
			}
		}
	}
	for _, input := range []nativeInput{
		{Action: "resolve", Drama: nativeDrama{ID: "huangdou:123", Source: sourceHongguo}},
		{Action: "resolve", Drama: nativeDrama{ID: "hongguo:123", Source: sourceHongguo}, Chapter: Chapter{Source: sourceHuangdou}},
		{Action: "resolve", Drama: nativeDrama{ID: "hongguo:123", Source: sourceHongguo}, Chapter: Chapter{ID: "huangdou:123:1"}},
	} {
		if !errors.Is(nativeAuthorizeInput(input), errNativeBuildSource) {
			t.Fatal("mismatched source passed native authorization")
		}
	}
	manager := downloadTestManager(t)
	input := downloadTestInput(1)
	input.Entries[0].Chapter.Source = sourceHuangdou
	if _, err := manager.enqueue(input); !errors.Is(err, errNativeBuildSource) {
		t.Fatalf("mismatched download source accepted: %v", err)
	}
}

func TestNativeBuildPreservesDownloadRecordsAcrossRestarts(t *testing.T) { // Connectguo: 全版本开放全部站源
	manager := downloadTestManager(t)
	records := []*nativeDownloadRecord{}
	for index, source := range []string{sourceHongguo, sourceHuangdou, sourceHuangguoVideo, sourceHuangguoAI, sourceCloudFront} {
		drama := nativeDrama{ID: source + ":123", Source: source, Title: "合成下载"}
		record := &nativeDownloadRecord{nativeDownloadJob: nativeDownloadJob{
			ID: nativeDownloadID(drama.ID, 1), Drama: drama, Index: 1,
			Chapter: Chapter{ID: "1", Source: source, CurrentEpisode: json.RawMessage("1")}, State: "queued", Created: int64(index),
		}}
		records = append(records, record)
	}
	data, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manager.root, "index.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	reopened := newNativeDownloads(manager.engine)
	t.Cleanup(reopened.close)
	jobs, err := reopened.snapshot()
	expected := 5 // Connectguo: 全版本开放全部站源
	if err != nil || len(jobs) != expected {
		t.Fatalf("wrong visible downloads: %v, %v", jobs, err)
	}
	if len(reopened.jobs) != len(records) {
		t.Fatal("changing edition discarded saved downloads")
	}
	started := make(chan string, 4)
	reopened.resolve = func(ctx context.Context, job nativeDownloadJob) (providerMedia, error) {
		started <- job.Drama.Source
		<-ctx.Done()
		return providerMedia{}, ctx.Err()
	}
	if err := reopened.control("", "resumeAll"); err != nil {
		t.Fatal(err)
	}
	workers := 2 // Connectguo: 全版本开放全部站源
	seen := map[string]bool{}
	for index := 0; index < workers; index++ {
		select {
		case source := <-started:
			seen[source] = true
		case <-time.After(5 * time.Second):
			t.Fatal("allowed download did not start")
		}
	}
	if !seen[sourceHongguo] || !seen[sourceHuangdou] { // Connectguo: 全版本开放全部站源
		t.Fatalf("wrong sources scheduled: %v", seen)
	}
	if err := reopened.control("", "pauseAll"); err != nil {
		t.Fatal(err)
	}
	reopened.close()
}
