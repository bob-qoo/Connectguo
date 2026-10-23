package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const hongguoBaseURL = "https://hongguoduanju.com"

func (d *Downloader) fetchHongguoDramas(ctx context.Context) ([]Drama, error) {
	initialized := hongguoCatalogInitialized(d.hongguoCatalogSnapshot())
	dramas, appErr := d.fetchHongguoAppCatalog(ctx)
	if len(dramas) > 0 || appErr == nil && initialized {
		return dramas, appErr
	}
	if ctx.Err() != nil {
		return dramas, ctx.Err()
	}
	webDramas, webErr := d.fetchHongguoWebDramas(ctx)
	if len(webDramas) > 0 && webErr == nil {
		return webDramas, nil
	}
	return webDramas, errors.Join(appErr, webErr)
}

func (d *Downloader) fetchHongguoWebDramas(ctx context.Context) ([]Drama, error) {
	routes := []struct {
		path string
		name string
	}{
		{path: "real-drama", name: "真人剧"},
		{path: "comic-drama", name: "漫剧"},
		{path: "ai-drama", name: "AI剧"},
		{path: "comic", name: "动漫"},
	}
	type routeResult struct {
		dramas []Drama
		err    error
	}
	results := make(chan routeResult, len(routes))
	for _, route := range routes {
		route := route
		go func() {
			dramas, err := d.fetchHongguoRoute(ctx, route.path, route.name)
			results <- routeResult{dramas: dramas, err: err}
		}()
	}
	seen := map[string]bool{}
	var out []Drama
	var failures []error
	for range routes {
		result := <-results
		if result.err != nil {
			failures = append(failures, result.err)
		}
		for _, drama := range result.dramas {
			if drama.ID != "" && !seen[drama.ID] {
				seen[drama.ID] = true
				out = append(out, drama)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DisplayTitle() < out[j].DisplayTitle() })
	return out, errors.Join(failures...)
}

func (d *Downloader) fetchHongguoRoute(ctx context.Context, route, category string) ([]Drama, error) {
	pages := d.cfg.MaxPagesPerSort
	if pages < 1 {
		pages = 1
	}
	var dramas []Drama
	seen := map[string]bool{}
	for page := 1; page <= pages; page++ {
		items, totalPages, err := d.fetchHongguoCategoryPage(ctx, route+"?page="+strconv.Itoa(page), category)
		if err != nil {
			return dramas, fmt.Errorf("%s: %w", category, err)
		}
		if totalPages > pages {
			pages = totalPages
			if pages > 500 {
				pages = 500
			}
		}
		reportLibraryProgress(ctx, sourceHongguo, items, nil, false)
		added := 0
		for _, drama := range items {
			if !seen[drama.ID] {
				seen[drama.ID] = true
				dramas = append(dramas, drama)
				added++
			}
		}
		if added == 0 || len(items) < 24 {
			break
		}
	}
	return dramas, nil
}

func (d *Downloader) fetchHongguoCategoryPage(ctx context.Context, route, category string) ([]Drama, int, error) {
	body, err := d.fetchProviderText(ctx, hongguoBaseURL+"/category/"+route, hongguoBaseURL+"/")
	if err != nil {
		return nil, 0, err
	}
	data := parseRouterData(body)
	page := routerLoaderMap(data, "category_page", "category_$")
	if len(page) == 0 || page["isSuccess"] == false {
		return nil, 0, errors.New("红果分类数据不可用，可能是页面结构或访问权限变化")
	}
	items := anyList(page["recommendList"])
	out := make([]Drama, 0, len(items))
	for _, item := range items {
		if dr := hongguoDramaFromAny(item, category); dr.ID != "" {
			out = append(out, dr)
		}
	}
	pages, _ := strconv.Atoi(mapString(nestedMap(page, "pagination"), "totalPages"))
	return out, pages, nil
}

func (d *Downloader) fetchHongguoChapters(ctx context.Context, sourceID string) (string, []Chapter, error) {
	sourceID = strings.TrimPrefix(strings.TrimSpace(sourceID), "hg-series-v1:")
	if sourceID == "" {
		return "", nil, fmt.Errorf("empty hongguo sourceID")
	}
	// 玩家 App 详情档处于「暂不可用」负缓存窗口时，跳过这条 3 次重试+退避的慢链、
	// 直走网页详情，提速详情页；窗口过期后自动恢复 App 优先。
	if d.hongguoPlayerSuspended() {
		if title, chapters, webErr := d.fetchHongguoWebChapters(ctx, sourceID); webErr == nil {
			return title, chapters, nil
		}
	}
	entry, appErr := d.hongguoAppDetail(ctx, sourceID)
	if appErr == nil {
		return entry.Drama.DisplayTitle(), append([]Chapter(nil), entry.Chapters...), nil
	}
	if ctx.Err() != nil {
		return "", nil, ctx.Err()
	}
	title, chapters, webErr := d.fetchHongguoWebChapters(ctx, sourceID)
	if webErr == nil {
		return title, chapters, nil
	}
	return "", nil, fmt.Errorf("App 详情失败: %v；网页详情失败: %w", publicError(appErr), webErr)
}

func (d *Downloader) fetchHongguoWebChapters(ctx context.Context, sourceID string) (string, []Chapter, error) {
	body, err := d.fetchProviderText(ctx, hongguoBaseURL+"/detail?series_id="+url.QueryEscape(sourceID), hongguoBaseURL+"/")
	if err != nil {
		return "", nil, err
	}
	data := parseRouterData(body)
	page := routerLoaderMap(data, "detail_page", "detail_")
	detail, _ := page["seriesDetail"].(map[string]any)
	if len(detail) == 0 {
		return "", nil, fmt.Errorf("hongguo detail is empty")
	}
	title := firstNonEmpty(mapString(detail, "series_name", "series_title", "name"), sourceID)
	vids := anyList(detail["vid_list"])
	chapters := make([]Chapter, 0, len(vids))
	for i, v := range vids {
		vid := strings.TrimSpace(fmt.Sprint(v))
		if vid == "" || vid == "<nil>" {
			continue
		}
		idx := i + 1
		chapters = append(chapters, Chapter{ID: providerChapterID(sourceHongguo, sourceID, vid), Source: sourceHongguo, Title: fmt.Sprintf("第%d集", idx), VideoURL: "hongguo-cenc://" + vid, CurrentEpisode: rawEpisode(idx)})
	}
	if len(chapters) == 0 {
		return title, nil, errors.New("红果详情没有返回剧集 ID")
	}
	return title, chapters, nil
}

func parseRouterData(raw string) map[string]any {
	idx := regexp.MustCompile(`(?s)(?:window\.)?_ROUTER_DATA\s*=\s*`).FindStringIndex(raw)
	if idx == nil {
		return nil
	}
	var data map[string]any
	dec := json.NewDecoder(strings.NewReader(raw[idx[1]:]))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return nil
	}
	return data
}

func (d *Downloader) resolveHongguoMedia(ctx context.Context, task Task) (providerMedia, error) {
	source, seriesID, ok := splitProviderDramaID(task.DramaID)
	if !ok || source != sourceHongguo {
		return providerMedia{}, fmt.Errorf("红果章节缺少剧 ID，请刷新剧库后重新加入队列")
	}
	videoID := strings.TrimPrefix(task.Chapter.VideoURL, "hongguo-cenc://")
	if !hongguoNumericID.MatchString(videoID) || !hongguoNumericID.MatchString(seriesID) {
		return providerMedia{}, fmt.Errorf("红果章节 ID 无效，请重新获取章节")
	}
	// 取流顺序：网页（免费试看范围内的明文 MP4）→ App CENC（试看范围外的加密集）→ 备用接口。
	// 网页取流内部有 accessible_episode_cnt 闸门：免费集直接命中明文直链，付费集返回"超出试看范围"
	// 错误，自动落到 App 取流，天然实现"前几集走网页、后面走 App"。
	media, pageErr := d.resolveHongguoWebMedia(ctx, seriesID, videoID)
	if pageErr == nil {
		return media, nil
	}
	if err := ctx.Err(); err != nil {
		return providerMedia{}, err
	}
	media, nativeErr := d.resolveHongguoAppMedia(ctx, videoID)
	if nativeErr == nil {
		return media, nil
	}
	if err := ctx.Err(); err != nil {
		return providerMedia{}, err
	}
	media, apiErr := d.resolveHongguoPlaybackAPI(ctx, seriesID, videoID)
	if apiErr == nil {
		return media, nil
	}
	return providerMedia{}, fmt.Errorf("红果网页取流失败：%v；App 取流失败：%v；备用取流失败：%w", publicError(pageErr), publicError(nativeErr), apiErr)
}

// resolveHongguoWebMedia 用官网 player 页取【免费试看范围】内的明文 MP4 直链。
//
// 网页版对普通用户只开放前 N 集（N=accessible_episode_cnt）的完整片源，且是
// 未加密的 MP4（无需 spade_a 密钥），适合「前几集」直接下载。超出该范围的集
// 网页只给试看片段，绝不能冒充正片——这类集请走 App CENC 取流。
//
// 直链不再藏在 _ROUTER_DATA 的 video_player_info（该字段已废弃），而是写在
// <script type="application/ld+json"> 的 VideoObject.contentUrl 里。
func (d *Downloader) resolveHongguoWebMedia(ctx context.Context, seriesID, videoID string) (providerMedia, error) {
	pageURL := hongguoBaseURL + "/player/" + url.PathEscape(seriesID) + "/" + url.PathEscape(videoID)
	body, err := d.fetchProviderText(ctx, pageURL, hongguoBaseURL+"/")
	if err != nil {
		return providerMedia{}, err
	}
	page := hongguoPlayerPage(parseRouterData(body))
	if len(page) == 0 {
		return providerMedia{}, errors.New("红果网页未返回目录数据")
	}
	if mapString(page, "vid") != videoID || mapString(page, "series_id") != seriesID {
		return providerMedia{}, fmt.Errorf("红果未返回所请求的剧集，可能仅允许网页试看")
	}
	// 免费试看闸门：本集下标若超出 accessible_episode_cnt，网页只有试看片段，不冒充正片。
	detail, _ := page["seriesDetail"].(map[string]any)
	if limit := mapInt(detail, "accessible_episode_cnt"); limit > 0 {
		if index := hongguoEpisodeIndex(anyList(detail["vid_list"]), videoID); index > limit {
			return providerMedia{}, fmt.Errorf("红果第 %d 集超出网页免费试看范围，请改用 App 取流", index)
		}
	}
	addresses := hongguoWebPublicAddresses(body, videoID)
	if len(addresses) == 0 {
		return providerMedia{}, errors.New("红果该集未提供公开播放地址，请改用 App 取流")
	}
	seo, _ := page["publicVideoSeo"].(map[string]any)
	duration, _ := strconv.ParseFloat(mapString(seo, "durationSeconds", "duration"), 64)
	height := mapInt(seo, "height")
	media := providerMedia{URL: addresses[0], Referer: d.providerBaseURL(sourceHongguo) + "/", Duration: time.Duration(duration * float64(time.Second)), Quality: height}
	for _, address := range addresses[1:] {
		media.Variants = append(media.Variants, providerMedia{URL: address, Referer: media.Referer, Duration: media.Duration, Quality: height})
	}
	return media, nil
}

// hongguoPlayerPage 取 player 页的 loaderData（形如 player_(series_id)/(vid)/page）。
// 优先选以 /page 结尾的那条（含 vid/series_id/publicVideoSeo），避免误取 layout。
func hongguoPlayerPage(data map[string]any) map[string]any {
	loader, _ := data["loaderData"].(map[string]any)
	if loader == nil {
		return nil
	}
	var fallback map[string]any
	for key, value := range loader {
		page, _ := value.(map[string]any)
		if len(page) == 0 || !strings.HasPrefix(key, "player_") {
			continue
		}
		if strings.HasSuffix(key, "/page") {
			return page
		}
		if fallback == nil {
			fallback = page
		}
	}
	return fallback
}

// hongguoEpisodeIndex 返回 videoID 在 vid_list 里的 1 基下标；找不到为 0。
func hongguoEpisodeIndex(vidList []any, videoID string) int {
	for i, v := range vidList {
		if strings.TrimSpace(fmt.Sprint(v)) == videoID {
			return i + 1
		}
	}
	return 0
}

// hongguoLDJSONPattern 匹配 <script type="application/ld+json"> 的内容体。
var hongguoLDJSONPattern = regexp.MustCompile(`<script\b[^>]*type\s*=\s*["']application/ld\+json["'][^>]*>([\s\S]*?)</script>`)

// hongguoWebPublicAddresses 从 player 页 HTML 的 ld+json VideoObject 里取指向本集的
// 公开 MP4 直链（/video/public/<vid>/xxx.mp4）。只收确实绑定本 vid 的直链，避免误抓
// 页面上推荐位/试看位的其它视频地址。
func hongguoWebPublicAddresses(body, videoID string) []string {
	var out []string
	seen := map[string]bool{}
	for _, match := range hongguoLDJSONPattern.FindAllStringSubmatch(body, -1) {
		rawJSON := match[1]
		node, err := parseLDJSONObject(rawJSON)
		if err != nil || fmt.Sprint(node["@type"]) != "VideoObject" {
			continue
		}
		// 内容必须确实指向本集：公开直链形如 /video/public/<vid>/360p.mp4。
		raw := strings.TrimSpace(fmt.Sprint(node["contentUrl"]))
		if raw == "" || raw == "<nil>" || !strings.Contains(raw, "/video/public/"+videoID+"/") {
			continue
		}
		address := hongguoNormalizeWebURL(raw)
		if address != "" && !seen[address] {
			seen[address] = true
			out = append(out, address)
		}
	}
	return out
}

// parseLDJSONObject 解析 ld+json 内容：先按纯 JSON 解析，失败再按 HTML 实体反转义后重试。
func parseLDJSONObject(raw string) (map[string]any, error) {
	decoded := strings.TrimSpace(raw)
	var node map[string]any
	if err := json.Unmarshal([]byte(decoded), &node); err == nil {
		return node, nil
	}
	decoded = html.UnescapeString(strings.TrimSpace(raw))
	var retry map[string]any
	if err := json.Unmarshal([]byte(decoded), &retry); err != nil {
		return nil, err
	}
	return retry, nil
}

// hongguoNormalizeWebURL 去掉 JS 转义（\u002F、\u0026、&amp;）与 Range 头，
// 让 CDN 回完整片而非分段。
func hongguoNormalizeWebURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, `\u002F`, "/")
	s = strings.ReplaceAll(s, `\u0026`, "&")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = regexp.MustCompile(`([?&])Range=[^&]*`).ReplaceAllString(s, "$1")
	return strings.TrimRight(s, "?&")
}

func hongguoDramaFromAny(v any, category string) Drama {
	m, ok := v.(map[string]any)
	if !ok {
		return Drama{}
	}
	vd, _ := m["video_data"].(map[string]any)
	if len(vd) == 0 {
		vd = m
	}
	sourceID := firstNonEmpty(mapString(vd, "series_id_str", "series_id"), mapString(m, "series_id_str", "series_id"), mapString(vd, "keyword"), mapString(m, "keyword"))
	if !hongguoNumericID.MatchString(sourceID) {
		return Drama{}
	}
	title := firstNonEmpty(mapString(vd, "series_title", "series_name", "title"), mapString(m, "series_name", "name"), sourceID)
	cover := firstNonEmpty(mapString(vd, "series_cover", "cover"), mapString(m, "series_cover"))
	intro := firstNonEmpty(mapString(vd, "series_intro", "video_desc"), mapString(m, "series_intro"))
	count := firstNonEmpty(mapString(vd, "episode_cnt"), mapString(m, "episode_cnt"))
	remark := firstNonEmpty(mapString(vd, "episode_right_text"), mapString(m, "episode_right_text"))
	releaseStatus := releaseStatusFromRemark(remark)
	if mapString(vd, "series_status") == "1" {
		releaseStatus = "finished"
	} else if mapString(vd, "series_status") == "0" {
		releaseStatus = "ongoing"
	}
	if remark == "" && count != "" {
		remark = "共" + count + "集"
	}
	tags := mapStringSlice(vd, "tags")
	for _, value := range anyList(vd["category_list"]) {
		item, _ := value.(map[string]any)
		if name := mapString(item, "name"); name != "" && !slices.Contains(tags, name) {
			tags = append(tags, name)
		}
	}
	var categories []struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(mapString(vd, "category_schema")), &categories) == nil {
		for _, item := range categories {
			if item.Name != "" && !slices.Contains(tags, item.Name) {
				tags = append(tags, item.Name)
			}
		}
	}
	genre := mapString(vd, "category_name", "categoryName", "category")
	if genre == "" && len(tags) > 0 {
		genre = tags[0]
	}
	return Drama{ID: providerDramaID(sourceHongguo, sourceID), Source: sourceHongguo, SourceID: sourceID, Title: title, Name: title, Desc: intro, Intro: intro, Cover: cover, CoverURL: cover, CategoryName: firstNonEmpty(genre, category), ChannelName: "红果", Remark: remark, TotalEpisode: count, EpisodeCount: count, Tags: tags, ReleaseStatus: releaseStatus, Score: mapString(vd, "score"), Views: mapString(vd, "series_play_cnt", "play_cnt"), Heat: hongguoHeat(vd), OnlineDate: hongguoOnlineDate(vd, time.Now())}
}

func nestedMap(v any, keys ...string) map[string]any {
	cur, _ := v.(map[string]any)
	for _, key := range keys {
		if cur == nil {
			return nil
		}
		cur, _ = cur[key].(map[string]any)
	}
	return cur
}

func routerLoaderMap(data map[string]any, names ...string) map[string]any {
	loader, _ := data["loaderData"].(map[string]any)
	for _, name := range names {
		if page, _ := loader[name].(map[string]any); len(page) > 0 {
			return page
		}
	}
	for key, value := range loader {
		for _, name := range names {
			if strings.TrimSuffix(name, "$") != "" && strings.HasPrefix(key, strings.TrimSuffix(name, "$")) {
				if page, _ := value.(map[string]any); len(page) > 0 {
					return page
				}
			}
		}
	}
	return nil
}

func anyList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case map[string]any:
		for _, key := range []string{"list", "items", "data"} {
			if out := anyList(x[key]); len(out) > 0 {
				return out
			}
		}
	}
	return nil
}
