package core

// 本文件是自定义源框架的「管道层」：从 guoapp3 的 duanju 框架中提取的
// 与具体站源无关的通用管道（HTTP 请求、JSON 提取、源规格解析、自定义源分发）。
// 不包含任何内置 duanju 站源定义——源规格只来自用户导入的自定义源注册表。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	duanjuMaxBodyBytes = 8 * 1024 * 1024
	duanjuUserAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
)


type duanjuSourceSpec struct {
	ID       string
	Name     string
	Base     string
	Kind     string
	Agent    string
	Referer  string
	Searcher bool
	Paged    bool
	Browser  bool
}

var duanjuMarkupPattern = regexp.MustCompile(`<[^>]{1,80}>`)

func duanjuPlainText(raw string) string {
	text := strings.TrimSpace(raw)
	if !strings.Contains(text, "<") {
		return text
	}
	text = duanjuMarkupPattern.ReplaceAllString(text, "")
	text = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'").Replace(text)
	return strings.Join(strings.Fields(text), " ")
}

// isDuanjuProviderSource 在本移植中只认自定义源（内置 duanju 站源未移植）。
func isDuanjuProviderSource(source string) bool {
	_, found := customMaccmsSpecFor(canonicalProviderSource(source))
	return found
}

// duanjuSourceSpecFor 只查自定义源注册表。
func duanjuSourceSpecFor(source string) (duanjuSourceSpec, bool) {
	return customMaccmsSpecFor(canonicalProviderSource(source))
}

func duanjuSourceName(source string) string {
	if spec, found := duanjuSourceSpecFor(source); found {
		return spec.Name
	}
	return canonicalProviderSource(source)
}

func (d *Downloader) duanjuBaseURL(source string) string {
	spec, found := duanjuSourceSpecFor(source)
	if !found {
		return ""
	}
	if configured := d.providerHosts[canonicalProviderSource(source)]; configured != "" {
		return strings.TrimRight(configured, "/")
	}
	return strings.TrimRight(spec.Base, "/")
}

func validDuanjuCategory(source, category string) bool {
	if category == "" {
		return true
	}
	if len(category) > 128 || strings.ContainsAny(category, "|\\\x00\r\n") {
		return false
	}
	// 自定义 MacCMS 源分类 ID 允许路径形式（含 /），仅做基础非法字符校验。
	return true
}

type duanjuCategory struct {
	ID   string
	Name string
}

func duanjuQueryEscape(value string) string { return url.QueryEscape(strings.TrimSpace(value)) }

func duanjuAbsolute(base, reference string) string {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return ""
	}
	if strings.HasPrefix(reference, "//") {
		return "https:" + reference
	}
	address, err := url.Parse(reference)
	if err != nil {
		return ""
	}
	if address.IsAbs() {
		return address.String()
	}
	origin, err := url.Parse(strings.TrimRight(base, "/") + "/")
	if err != nil {
		return ""
	}
	return origin.ResolveReference(address).String()
}

type duanjuRequest struct {
	Source  string
	Method  string
	Address string
	Referer string
	Agent   string
	Headers map[string]string
	Body    []byte
	Form    url.Values
	Timeout time.Duration
}


func (d *Downloader) duanjuDo(ctx context.Context, request duanjuRequest) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method == "" {
		method = http.MethodGet
	}
	address := strings.TrimSpace(request.Address)
	if address == "" {
		return nil, errors.New("站源地址为空")
	}
	body := request.Body
	if len(request.Form) > 0 && len(body) == 0 {
		body = []byte(request.Form.Encode())
	}
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, method, address, reader)
	if err != nil {
		return nil, err
	}
	agent := firstNonEmpty(request.Agent, duanjuUserAgent)
	httpRequest.Header.Set("User-Agent", agent)
	httpRequest.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	httpRequest.Header.Set("Accept", "*/*")
	if referer := strings.TrimSpace(request.Referer); referer != "" {
		httpRequest.Header.Set("Referer", referer)
	}
	if len(request.Form) > 0 && len(request.Body) == 0 {
		httpRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for key, value := range request.Headers {
		if strings.EqualFold(key, "Host") || strings.EqualFold(key, "Content-Length") {
			continue
		}
		httpRequest.Header.Set(key, value)
	}
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = providerTimeout
	}
	response, err := d.doCatalogRequestWithTimeout(httpRequest, timeout)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, duanjuMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > duanjuMaxBodyBytes {
		return nil, errors.New("站源返回内容过大")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &requestBackoff{status: response.StatusCode, host: duanjuHostOf(address), until: time.Now().Add(time.Minute)}
	}
	return payload, nil
}

func duanjuHostOf(address string) string {
	parsed, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func (d *Downloader) duanjuJSON(ctx context.Context, request duanjuRequest) (map[string]any, error) {
	payload, err := d.duanjuDo(ctx, request)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, errors.New("站源返回的数据格式无法识别")
	}
	return decoded, nil
}

func duanjuFindString(node map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, found := node[key]; found {
			if text := nativeText(value); text != "" {
				return text
			}
		}
	}
	return ""
}

func duanjuFindInt(node map[string]any, keys ...string) int {
	for _, key := range keys {
		if value, found := node[key]; found {
			switch typed := value.(type) {
			case json.Number:
				if number, err := typed.Int64(); err == nil {
					return int(number)
				}
			case float64:
				return int(typed)
			case string:
				if number, err := parseDuanjuNumber(typed); err == nil {
					return number
				}
			}
		}
	}
	return 0
}

func parseDuanjuNumber(value string) (int, error) {
	trimmed := strings.TrimSpace(value)
	if number, err := strconv.Atoi(trimmed); err == nil {
		return number, nil
	}
	trimmed = strings.TrimSuffix(strings.TrimSuffix(trimmed, "集"), "全集")
	trimmed = strings.TrimSpace(trimmed)
	if number, err := strconv.Atoi(trimmed); err == nil {
		return number, nil
	}
	return 0, errors.New("不是数字")
}

func duanjuFindMap(node map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value, found := node[key]; found {
			if mapped, ok := value.(map[string]any); ok {
				return mapped
			}
		}
	}
	return nil
}

func duanjuFindList(node map[string]any, keys ...string) []any {
	for _, key := range keys {
		if value, found := node[key]; found {
			if listed, ok := value.([]any); ok {
				return listed
			}
		}
	}
	return nil
}

func duanjuMapList(value any) []map[string]any {
	listed, ok := value.([]any)
	if !ok {
		return nil
	}
	rows := make([]map[string]any, 0, len(listed))
	for _, entry := range listed {
		if mapped, ok := entry.(map[string]any); ok {
			rows = append(rows, mapped)
		}
	}
	return rows
}

func duanjuStringList(value any) []string {
	listed, ok := value.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(listed))
	for _, entry := range listed {
		switch typed := entry.(type) {
		case string:
			if text := strings.TrimSpace(typed); text != "" {
				values = append(values, text)
			}
		case map[string]any:
			if text := duanjuFindString(typed, "name", "title", "class_name"); text != "" {
				values = append(values, text)
			}
		}
	}
	return values
}

func duanjuCSV(values []string) string {
	return strings.Join(values, ", ")
}

func duanjuCover(base string, node map[string]any, keys ...string) string {
	for _, key := range keys {
		if address := providerCoverAddress(node[key], base); address != "" {
			return address
		}
	}
	return ""
}

func duanjuReleaseStatus(raw string, total, current int) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "over", "finished", "completed", "2", "已完结", "完结":
		return "finished"
	case "ongoing", "serializing", "1", "连载中", "更新中":
		return "ongoing"
	}
	if total > 0 && current >= total {
		return "finished"
	}
	if current > 0 {
		return "ongoing"
	}
	return ""
}

func duanjuEpisodeNumber(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if number := episodeIndex(raw, 0); number > 0 {
		return number
	}
	if number, err := strconv.Atoi(raw); err == nil && number > 0 {
		return number
	}
	if fallback > 0 {
		return fallback
	}
	return 1
}

func duanjuChapter(source, sourceID string, number int, title, videoURL, pageURL, referer string) Chapter {
	if number <= 0 {
		number = 1
	}
	if strings.TrimSpace(title) == "" {
		title = "第" + strconv.Itoa(number) + "集"
	}
	return Chapter{
		ID:             providerChapterID(source, sourceID, strconv.Itoa(number)),
		Source:         source,
		Title:          truncate(title, 256),
		CurrentEpisode: rawEpisode(number),
		VideoURL:       strings.TrimSpace(videoURL),
		PageURL:        strings.TrimSpace(pageURL),
		Referer:        strings.TrimSpace(referer),
	}
}

func duanjuChapterKey(number int) string { return strconv.Itoa(number) }

func sortDuanjuChapters(chapters []Chapter) {
	sort.SliceStable(chapters, func(i, j int) bool {
		left, _ := strconv.Atoi(chapters[i].EpisodeString(i + 1))
		right, _ := strconv.Atoi(chapters[j].EpisodeString(j + 1))
		return left < right
	})
}

// ---- 自定义源分发（原 duanju dispatch 的自定义分支） ----

func (d *Downloader) fetchDuanjuCategories(ctx context.Context, source string) ([]nativeCategory, error) {
	source = canonicalProviderSource(source)
	if !isDuanjuProviderSource(source) {
		return nil, errors.New("该站源不支持分类读取")
	}
	if isCustomMaccmsSource(source) {
		return d.fetchMaccmsCustomCategories(ctx, source)
	}
	return nil, errors.New("该站源暂未提供分类")
}

func (d *Downloader) fetchDuanjuCatalogPage(ctx context.Context, source string, page int, category string) ([]Drama, bool, error) {
	source = canonicalProviderSource(source)
	if isCustomMaccmsSource(source) {
		return d.fetchMaccmsCatalogPage(ctx, source, page, category)
	}
	return nil, false, errors.New("该站源暂未接入目录")
}

func (d *Downloader) fetchDuanjuDetail(ctx context.Context, source, sourceID string) (Drama, []Chapter, error) {
	source = canonicalProviderSource(source)
	if isCustomMaccmsSource(source) {
		return d.fetchMaccmsDetail(ctx, source, sourceID)
	}
	return Drama{}, nil, errors.New("该站源暂未接入详情")
}

func (d *Downloader) searchDuanju(ctx context.Context, source, query string) ([]Drama, error) {
	source = canonicalProviderSource(source)
	if isCustomMaccmsSource(source) {
		return d.searchMaccms(ctx, source, query)
	}
	return nil, errors.New("该站源不支持在线搜索")
}

func duanjuSupportsSearch(source string) bool {
	spec, found := duanjuSourceSpecFor(source)
	return found && spec.Searcher
}

func duanjuSupportsPaging(source string) bool {
	spec, found := duanjuSourceSpecFor(source)
	return found && spec.Paged
}

func (d *Downloader) resolveDuanjuMedia(ctx context.Context, task Task) (providerMedia, error) {
	source := canonicalProviderSource(task.Chapter.Source)
	name := duanjuSourceName(source)
	base := d.duanjuBaseURL(source)
	if base == "" {
		return providerMedia{}, errors.New("站源地址不可用")
	}
	// XBPQ 规则源：播放页按规则「跳转播放链接」→ player_data → 嗅探兜底。
	if rule, ok := customXBPQRule(source); ok {
		return d.xbpqResolveMedia(ctx, task, rule, base, name)
	}
	address := strings.TrimSpace(task.Chapter.VideoURL)
	pageURL := strings.TrimSpace(task.Chapter.PageURL)
	referer := firstNonEmpty(task.Chapter.Referer, base+"/")
	if !isProviderHTTPMediaURL(address) || !duanjuLooksLikeMedia(address) {
		if pageURL == "" {
			if source, sourceID, valid := splitProviderDramaID(task.DramaID); valid {
				if _, chapters, err := d.fetchDuanjuDetail(ctx, source, sourceID); err == nil {
					for _, fresh := range chapters {
						if fresh.ID == task.Chapter.ID {
							address = fresh.VideoURL
							pageURL = fresh.PageURL
							break
						}
					}
				}
			}
		}
		if pageURL != "" {
			resolved, err := d.resolveDuanjuWebPage(ctx, source, pageURL, referer)
			if err != nil {
				return providerMedia{}, err
			}
			prepared, err := d.prepareWebProviderMedia(ctx, resolved, name)
			if err == nil {
				return prepared, nil
			}
			// 默认线路可能被源站下架或限速（返回 404 / 403），自动改用同一集的其他线路。
			if fallback, ok := d.prepareWebProviderRouteFallback(ctx, resolved, name); ok {
				return fallback, nil
			}
			return providerMedia{}, err
		}
	}
	if !isProviderHTTPMediaURL(address) {
		return providerMedia{}, fmt.Errorf("%s未返回有效播放地址，请刷新章节后重试", name)
	}
	return d.prepareWebProviderMedia(ctx, providerMedia{URL: address, Referer: referer}, name)
}

// prepareWebProviderRouteFallback 在主线路拿不到可播内容时，依次试用备选线路，
// 首个可播线路提为主地址，其余线路（含原主线路）继续留在备选列表里供手动切换。
func (d *Downloader) prepareWebProviderRouteFallback(ctx context.Context, media providerMedia, name string) (providerMedia, bool) {
	if len(media.Variants) == 0 {
		return providerMedia{}, false
	}
	for index, variant := range media.Variants {
		if !isProviderHTTPMediaURL(variant.URL) {
			continue
		}
		prepared, err := d.prepareWebProviderMedia(ctx, providerMedia{URL: variant.URL, Referer: variant.Referer}, name)
		if err != nil {
			continue
		}
		remaining := make([]providerMedia, 0, len(media.Variants))
		remaining = append(remaining, media.Variants[:index]...)
		remaining = append(remaining, media.Variants[index+1:]...)
		if isProviderHTTPMediaURL(media.URL) {
			remaining = append([]providerMedia{{URL: media.URL, Referer: media.Referer}}, remaining...)
		}
		prepared.Variants = remaining
		return prepared, true
	}
	return providerMedia{}, false
}

func duanjuLooksLikeMedia(address string) bool {
	lower := strings.ToLower(address)
	return strings.Contains(lower, ".m3u8") || strings.Contains(lower, ".mp4") || strings.Contains(lower, ".flv")
}

func (d *Downloader) resolveDuanjuWebPage(ctx context.Context, source, pageURL, referer string) (providerMedia, error) {
	body, err := d.fetchProviderText(ctx, pageURL, referer)
	if err != nil {
		return providerMedia{}, err
	}
	address := maccmsNormalizePlaybackURL(maccmsPlayerURL(body))
	if isProviderHTTPMediaURL(address) {
		media := providerMedia{URL: address, Referer: pageURL}
		// 同一集通常有多条线路（m3u8/云播/各视频站），详情只展示一条线路的集数，
		// 其余线路在这里补齐，播放页就能用「线路 N」切换。
		media.Variants = d.collectMaccmsRouteVariants(ctx, pageURL, address)
		// MacCMS 混合资源站：主线路可能是第三方 HTML 页而非媒体直链；有媒体形态的
		// 备选线路时与其互换，第三方线路降级留在备选里供手动切换。
		if !duanjuLooksLikeMedia(address) {
			for index, variant := range media.Variants {
				if duanjuLooksLikeMedia(variant.URL) {
					remaining := make([]providerMedia, 0, len(media.Variants))
					remaining = append(remaining, media.Variants[:index]...)
					remaining = append(remaining, media.Variants[index+1:]...)
					media.URL, media.Referer = variant.URL, variant.Referer
					media.Variants = append([]providerMedia{{URL: address, Referer: pageURL}}, remaining...)
					break
				}
			}
		}
		return media, nil
	}
	// 加密串模板：走 /static/player/<from>.js 的 iframe 云解析拿真实 m3u8。
	fields := maccmsPlayerFields(body)
	encrypted := fields["url"]
	if encrypted != "" && !strings.HasPrefix(strings.ToLower(encrypted), "http") {
		if parsed := d.resolveMaccmsCloudParse(ctx, body, encrypted, pageURL); parsed != "" {
			return providerMedia{URL: maccmsNormalizePlaybackURL(parsed), Referer: pageURL}, nil
		}
		// 当前线路云解析失败：换该站其他线路逐条重试。
		if alt := d.resolveMaccmsAlternateRoutes(ctx, pageURL); alt != "" {
			return providerMedia{URL: maccmsNormalizePlaybackURL(alt), Referer: pageURL}, nil
		}
	}
	if address == "" || !isProviderHTTPMediaURL(address) {
		return providerMedia{}, fmt.Errorf("%s未返回有效播放地址，请刷新章节后重试", duanjuSourceName(source))
	}
	return providerMedia{URL: address, Referer: pageURL}, nil
}

var (
	maccmsPlaySIDSegment = regexp.MustCompile(`(?i)sid/(\d+)`)
	maccmsPlayMyuiTail   = regexp.MustCompile(`(?i)^(.*/\d+)-(\d+)-(\d+\.html)$`)
)

const (
	// maccmsMaxRouteSID 是探测线路号的上限（站点线路通常不超过 10 条）。
	maccmsMaxRouteSID = 12
	// maccmsMaxRouteVariants 是最多采集的备选线路数量（不含当前线路）。
	maccmsMaxRouteVariants = 3
)

// maccmsAlternateRoutePaths 由当前播放页 URL 生成同集其他线路（sid）的页面路径。
func maccmsAlternateRoutePaths(pageURL string) (siteBase string, paths []string) {
	parsed, err := url.Parse(pageURL)
	if err != nil || parsed.Host == "" {
		return "", nil
	}
	currentSID := -1
	if matches := maccmsPlaySIDSegment.FindStringSubmatch(parsed.Path); len(matches) > 1 {
		currentSID, _ = strconv.Atoi(matches[1])
		for sid := 1; sid <= maccmsMaxRouteSID; sid++ {
			if sid == currentSID {
				continue
			}
			paths = append(paths, maccmsPlaySIDSegment.ReplaceAllString(parsed.Path, "sid/"+strconv.Itoa(sid)))
		}
	} else if matches := maccmsPlayMyuiTail.FindStringSubmatch(parsed.Path); len(matches) > 1 {
		currentSID, _ = strconv.Atoi(matches[2])
		for sid := 1; sid <= maccmsMaxRouteSID; sid++ {
			if sid == currentSID {
				continue
			}
			paths = append(paths, matches[1]+"-"+strconv.Itoa(sid)+"-"+matches[3])
		}
	}
	if len(paths) == 0 {
		return "", nil
	}
	return parsed.Scheme + "://" + parsed.Host, paths
}

// resolveMaccmsAlternateRoutes 在当前播放页云解析失败时，尝试同一剧集的其他线路（sid）。
func (d *Downloader) resolveMaccmsAlternateRoutes(ctx context.Context, pageURL string) string {
	siteBase, paths := maccmsAlternateRoutePaths(pageURL)
	if siteBase == "" {
		return ""
	}
	for _, path := range paths {
		altURL := siteBase + path
		altBody, err := d.fetchProviderText(ctx, altURL, pageURL)
		if err != nil || altBody == "" {
			continue
		}
		altFields := maccmsPlayerFields(altBody)
		encrypted := altFields["url"]
		if encrypted == "" || strings.HasPrefix(strings.ToLower(encrypted), "http") {
			continue
		}
		if parsedResult := d.resolveMaccmsCloudParse(ctx, altBody, encrypted, altURL); parsedResult != "" {
			return parsedResult
		}
	}
	return ""
}

// maccmsResolveRoutePage 抓取一条线路的播放页并返回可播放地址（明文直取，加密串走云解析）。
func (d *Downloader) maccmsResolveRoutePage(ctx context.Context, altURL, referer string) string {
	body, err := d.fetchProviderText(ctx, altURL, referer)
	if err != nil || body == "" {
		return ""
	}
	if address := maccmsNormalizePlaybackURL(maccmsPlayerURL(body)); isProviderHTTPMediaURL(address) {
		return address
	}
	fields := maccmsPlayerFields(body)
	encrypted := fields["url"]
	if encrypted == "" || strings.HasPrefix(strings.ToLower(encrypted), "http") {
		return ""
	}
	if parsed := d.resolveMaccmsCloudParse(ctx, body, encrypted, altURL); parsed != "" {
		return maccmsNormalizePlaybackURL(parsed)
	}
	return ""
}

// collectMaccmsRouteVariants 采集同一集其他线路的播放地址，作为播放页的备选线路。
func (d *Downloader) collectMaccmsRouteVariants(ctx context.Context, pageURL, primary string) []providerMedia {
	siteBase, paths := maccmsAlternateRoutePaths(pageURL)
	if siteBase == "" {
		return nil
	}
	if len(paths) > maccmsMaxRouteVariants {
		paths = paths[:maccmsMaxRouteVariants]
	}
	// 备选线路只是锦上添花，给一个总时限，避免拖慢主线路的解析返回。
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	found := make([]string, len(paths))
	var waiter sync.WaitGroup
	for index, path := range paths {
		waiter.Add(1)
		go func(slot int, address string) {
			defer waiter.Done()
			found[slot] = d.maccmsResolveRoutePage(ctx, address, pageURL)
		}(index, siteBase+path)
	}
	waiter.Wait()
	seen := map[string]bool{}
	if primary != "" {
		seen[primary] = true
	}
	var variants []providerMedia
	for index, address := range found {
		if !isProviderHTTPMediaURL(address) || seen[address] {
			continue
		}
		seen[address] = true
		variants = append(variants, providerMedia{URL: address, Referer: siteBase + paths[index]})
	}
	return variants
}
