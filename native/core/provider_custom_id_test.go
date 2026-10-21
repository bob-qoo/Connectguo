package core

import (
	"net/url"
	"testing"
)

// 回归：自定义源剧集 ID 形如 custom:<hash>:<sourceID>，切分必须保留完整的
// custom:<hash> 作源，否则注册表查不到，详情/分集/播放全断（目录不受影响，
// 因为目录阶段不切分 ID）。2026-10-21 由 91crdjai 自定义源实测发现。
func TestSplitProviderDramaIDCustomSource(t *testing.T) {
	engine, err := newNativeEngine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// 注册一个带 XBPQ 规则的自定义源（规则只需含主页url 即可通过识别）。
	rule := `{"主页url":"https://example.com","分类url":"https://example.com/{cateId}/page/{catePg}/","数组":"<a&&</a>","标题":"<h3>&&</h3>","链接":"href=\"&&\""}`
	record, err := engine.addCustomMaccmsSource("示例源", "", rule)
	if err != nil {
		t.Fatal(err)
	}
	nativeState.Lock()
	previous := nativeState.engine
	nativeState.engine = engine
	nativeState.Unlock()
	defer func() {
		nativeState.Lock()
		nativeState.engine = previous
		nativeState.Unlock()
	}()

	link := "https://example.com/duanju/4714-mushanggonglvedi1ji/"
	sourceID := xbpqIDFromLink(link, false)
	if sourceID == "" || sourceID[0] != 'u' {
		t.Fatalf("xbpqIDFromLink = %q, want u-prefixed id", sourceID)
	}
	dramaID := providerDramaID(record.ID, sourceID)
	source, sid, ok := splitProviderDramaID(dramaID)
	if !ok {
		t.Fatalf("splitProviderDramaID(%q) invalid", dramaID)
	}
	if source != record.ID {
		t.Fatalf("source = %q, want %q", source, record.ID)
	}
	if sid != sourceID {
		t.Fatalf("sourceID = %q, want %q", sid, sourceID)
	}
	if got := xbpqLinkFromID("https://example.com", sid); got != link {
		t.Fatalf("xbpqLinkFromID = %q, want %q", got, link)
	}
	// 内置源切分不受影响。
	if source, sid, ok := splitProviderDramaID("hongguo:123"); !ok || source != "hongguo" || sid != "123" {
		t.Fatalf("builtin split = %q %q %v", source, sid, ok)
	}
	// 残缺的自定义源 ID 仍应拒绝。
	if _, _, ok := splitProviderDramaID("custom:abc"); ok {
		t.Fatal("splitProviderDramaID accepted incomplete custom id")
	}
	_ = url.QueryEscape
}
