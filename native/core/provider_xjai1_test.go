package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// 离线夹具：卡片结构取自 xjai1 剧场页真实卡片（2026-10-21 实测），
// 覆盖免费卡（保留）、金币卡与 VIP 卡（排除）。

const xjai1CatalogFixture = `
<article class="g-theater-catalog-card">
  <a href="/dramas/aaaaaaaa-1111-2222-3333-444444444444">
    <img src="https://mediacms.xonyok.cn/catalog-covers/2026/aa.jpg">
    <span class="g-drama-charge-badge is-free is-cover">免费</span>
    <strong>我的豆包女友</strong>
    <small><span>更新至48集</span></small>
  </a>
</article>
<article class="g-theater-catalog-card">
  <a href="/dramas/bbbbbbbb-1111-2222-3333-444444444444">
    <img src="https://mediacms.xonyok.cn/catalog-covers/2026/bb.jpg">
    <span class="badge coin">金币</span>
    <strong>金币剧</strong>
    <small><span>更新至10集</span></small>
  </a>
</article>
<article class="g-theater-catalog-card">
  <a href="/dramas/cccccccc-1111-2222-3333-444444444444">
    <img src="https://mediacms.xonyok.cn/catalog-covers/2026/cc.jpg">
    <span class="badge vip">VIP</span>
    <strong>VIP 剧</strong>
    <small><span>更新至20集</span></small>
  </a>
</article>
`

func TestParseXjai1CatalogPage(t *testing.T) {
	items, raw := parseXjai1CatalogPage(xjai1CatalogFixture)
	if raw != 3 {
		t.Fatalf("want 3 raw cards, got %d", raw)
	}
	if len(items) != 1 {
		t.Fatalf("want 1 free drama, got %d", len(items))
	}
	got := items[0]
	if got.ID != "xjai1:aaaaaaaa-1111-2222-3333-444444444444" {
		t.Fatalf("wrong id: %s", got.ID)
	}
	if got.Title != "我的豆包女友" {
		t.Fatalf("wrong title: %s", got.Title)
	}
	if got.CoverURL != "https://mediacms.xonyok.cn/catalog-covers/2026/aa.jpg" {
		t.Fatalf("wrong cover: %v", got.CoverURL)
	}
	if got.TotalEpisode != 48 {
		t.Fatalf("wrong episode count: %v", got.TotalEpisode)
	}
}

const xjai1EpisodesFixture = `{"data":[
{"commerce":{"episode_id":"e1","charge_type":"free","can_play":true},"id":"e1","drama_id":"d1","title":"剧\n第1集","episode_number":1,"duration_seconds":300},
{"commerce":{"episode_id":"e2","charge_type":"coin","can_play":false},"id":"e2","drama_id":"d1","title":"剧\n第2集","episode_number":2,"duration_seconds":310}
],"request_id":"x"}`

func TestParseXjai1Episodes(t *testing.T) {
	episodes, err := parseXjai1Episodes([]byte(xjai1EpisodesFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes) != 2 {
		t.Fatalf("want 2 episodes, got %d", len(episodes))
	}
	if episodes[0].EpisodeNumber != 1 || episodes[0].Commerce == nil || !episodes[0].Commerce.CanPlay {
		t.Fatalf("first episode mismatch: %+v", episodes[0])
	}
	if episodes[1].Commerce == nil || episodes[1].Commerce.CanPlay {
		t.Fatalf("second episode should be unplayable: %+v", episodes[1])
	}
}

const xjai1SessionFixture = `{"data":{
"play_mode":"full","trial_seconds":0,"id":"ply_test","drama_id":"d1","episode_id":"e1","status":"active",
"media":{"kind":"direct_hls","manifest_url":"https://mediacms.xonyok.cn/media-delivery/direct/v1/platform/1/abc/123/720p.m3u8?ml_sign=x&t=123","expires_at":"2026-10-21T10:15:09Z"}
},"request_id":"y"}`

func TestParseXjai1Session(t *testing.T) {
	session, err := parseXjai1Session([]byte(xjai1SessionFixture))
	if err != nil {
		t.Fatal(err)
	}
	if session.PlayMode != "full" || session.Kind != "direct_hls" {
		t.Fatalf("session mismatch: %+v", session)
	}
	if !strings.Contains(session.ManifestURL, "mediacms.xonyok.cn") || !strings.HasSuffix(strings.Split(session.ManifestURL, "?")[0], ".m3u8") {
		t.Fatalf("bad manifest url: %s", session.ManifestURL)
	}
}

func TestParseXjai1ChapterID(t *testing.T) {
	drama, episode, ok := parseXjai1ChapterID("xjai1:drama-uuid:episode-uuid")
	if !ok || drama != "drama-uuid" || episode != "episode-uuid" {
		t.Fatalf("parse failed: %q %q %v", drama, episode, ok)
	}
	if _, _, ok := parseXjai1ChapterID("xjai1:only-one"); ok {
		t.Fatal("malformed chapter id must not parse")
	}
	if _, _, ok := parseXjai1ChapterID("hongguo:1:2"); ok {
		t.Fatal("foreign source chapter id must not parse as xjai1")
	}
}

func TestValidXjai1Category(t *testing.T) {
	if !validNativeCategory(sourceXjai1, "mingxinghuanlian") {
		t.Fatal("known tag slug must be valid")
	}
	if validNativeCategory(sourceXjai1, "not-a-tag") {
		t.Fatal("unknown slug must be invalid")
	}
	if !isBuiltinProviderSource(sourceXjai1) {
		t.Fatal("xjai1 must be a builtin provider source")
	}
	if canonicalProviderSource("xjai1.com") != sourceXjai1 {
		t.Fatal("xjai1.com must canonicalize to xjai1")
	}
}

func xjai1TestServer(t *testing.T, freeOnPage int) *httptest.Server {
	t.Helper()
	coinCard := func(i int) string {
		return fmt.Sprintf(`<article class="g-theater-catalog-card"><a href="/dramas/coin-%d"><img src="https://example.test/c.jpg"><span class="badge coin">金币</span><strong>金币剧%d</strong><small><span>更新至10集</span></small></a></article>`, i, i)
	}
	freeCard := `<article class="g-theater-catalog-card"><a href="/dramas/free-1"><img src="https://example.test/f.jpg"><span class="g-drama-charge-badge is-free is-cover">免费</span><strong>免费剧</strong><small><span>更新至5集</span></small></a></article>`
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var page int
		if _, err := fmt.Sscanf(r.URL.Path, "/theater/page/%d/", &page); err != nil || page < 1 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var b strings.Builder
		if page == freeOnPage {
			b.WriteString(freeCard)
		} else {
			for i := 0; i < 42; i++ {
				b.WriteString(coinCard(i))
			}
		}
		w.Write([]byte(b.String()))
	}))
}

// 回归：剧场第 1 页过滤后为 0 时，目录必须自动往后翻到有内容的那页，
// 而不是报“站源暂未返回剧集”。
func TestXjai1CatalogAutoAdvance(t *testing.T) {
	upstream := xjai1TestServer(t, 2)
	defer upstream.Close()
	old := xjai1BaseURL
	xjai1BaseURL = upstream.URL
	defer func() { xjai1BaseURL = old }()
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.nativeCatalog(context.Background(), nativeInput{Source: sourceXjai1, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "xjai1:free-1" || result.Items[0].Title != "免费剧" {
		t.Fatalf("unexpected items: %+v", result.Items)
	}
	if result.Page != 2 {
		t.Fatalf("want page 2 (first non-empty page), got %d", result.Page)
	}
}

// 回归：连续多页都过滤为空时，目录也不应报错，只返回空结果。
func TestXjai1CatalogAllEmptyNoError(t *testing.T) {
	upstream := xjai1TestServer(t, -1)
	defer upstream.Close()
	old := xjai1BaseURL
	xjai1BaseURL = upstream.URL
	defer func() { xjai1BaseURL = old }()
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := engine.nativeCatalog(context.Background(), nativeInput{Source: sourceXjai1, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 0 {
		t.Fatalf("want 0 items, got %d", len(result.Items))
	}
}

// 以下真站探针默认跳过，仅 XJAI1_LIVE=1 时跑。
func xjai1Live(t *testing.T) {
	t.Helper()
	if os.Getenv("XJAI1_LIVE") == "" {
		t.Skip("set XJAI1_LIVE=1 to run live xjai1 probes")
	}
}

func TestXjai1LivePlayback(t *testing.T) {
	xjai1Live(t)
	d := &Downloader{}
	// 用当前可播的分集（站内免费/试看状态会轮换，固定用第 1 集）。
	task := Task{
		DramaID: "xjai1:c9ef625c-064d-4589-8e94-d86a4a74646e",
		Chapter: Chapter{
			ID:     "xjai1:c9ef625c-064d-4589-8e94-d86a4a74646e:87a3e5ce-d0e8-4e5f-bf1e-ae92492aea57",
			Source: "xjai1",
		},
		Index: 0,
	}
	media, err := d.resolveXjai1Media(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(media.URL, "mediacms.xonyok.cn") || !strings.Contains(media.URL, ".m3u8") {
		t.Fatalf("unexpected manifest url: %s", media.URL)
	}
	if media.Referer != "https://xjai1.com/" {
		t.Fatalf("unexpected referer: %s", media.Referer)
	}
}

func TestXjai1LiveDetail(t *testing.T) {
	xjai1Live(t)
	d := &Downloader{}
	raw, chapters, err := d.fetchXjai1Detail(t.Context(), "c9ef625c-064d-4589-8e94-d86a4a74646e")
	if err != nil {
		t.Fatal(err)
	}
	if raw.ID != "xjai1:c9ef625c-064d-4589-8e94-d86a4a74646e" || raw.Title == "" {
		t.Fatalf("detail mismatch: %+v", raw)
	}
	if len(chapters) < 1 {
		t.Fatalf("want at least 1 playable chapter, got %d", len(chapters))
	}
	if chapters[0].Title != "第1集" {
		t.Fatalf("first chapter title: %s", chapters[0].Title)
	}
}

func TestXjai1LiveCatalog(t *testing.T) {
	xjai1Live(t)
	d := &Downloader{}
	// 免费内容会轮换，单页可能为 0：往后翻几页找第一部免费剧验证形态，
	// 几页都空说明当前免费架为空，跳过而非判失败。
	for page := 1; page <= 5; page++ {
		items, _, err := d.fetchXjai1CatalogPage(t.Context(), page, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			continue
		}
		for _, item := range items {
			if !strings.HasPrefix(item.ID, "xjai1:") || item.Title == "" {
				t.Fatalf("bad catalog item: %+v", item)
			}
		}
		t.Logf("theater page %d: %d free dramas", page, len(items))
		return
	}
	t.Skip("no free dramas on theater pages 1-5 right now (site rotation)")
}
