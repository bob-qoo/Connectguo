package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// xjai1（香蕉短剧）内置源站：目录走站内剧场/题材 HTML（与旧 XBPQ 规则同源，
// 仅免费卡片进目录），分集与播放走站内 JSON API。播放必须先 POST 创建会话，
// 会话返回的 direct_hls manifest 内嵌 AES 密钥、每条分片自带签名，
// 故 App 只需原样播 manifest，无需自行拼地址或取密钥。

const sourceXjai1 = "xjai1"

const xjai1UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// var 而非 const：测试用 httptest 覆盖基址，验证空页自动往后翻。
var xjai1BaseURL = "https://xjai1.com"

var nativeXjai1Categories = []nativeCategory{
	{ID: "mingxinghuanlian", Name: "明星换脸"},
	{ID: "yingshimogai", Name: "影视魔改"},
	{ID: "dongmanyouxi", Name: "动漫游戏"},
	{ID: "jiatingluanlun", Name: "家庭乱伦"},
	{ID: "chuanyuechongsheng", Name: "穿越重生"},
	{ID: "xitongyineng", Name: "系统异能"},
	{ID: "xianxiaxiuzhen", Name: "仙侠修真"},
	{ID: "guzhuangquanmou", Name: "古装权谋"},
	{ID: "lingyiqihuan", Name: "灵异奇幻"},
	{ID: "tongxingticai", Name: "同性题材"},
	{ID: "xingzhuanweiniang", Name: "性转伪娘"},
	{ID: "hunyinbeipan", Name: "婚姻背叛"},
	{ID: "zhipeidiaojiao", Name: "支配调教"},
	{ID: "toupaitoukui", Name: "偷拍偷窥"},
	{ID: "gonggongluchu", Name: "公共露出"},
	{ID: "xuanyimaoxian", Name: "悬疑冒险"},
	{ID: "xiangcunyanqing", Name: "乡村艳情"},
	{ID: "xiaoyuanqingchun", Name: "校园青春"},
	{ID: "dushizhichang", Name: "都市职场"},
	{ID: "yiliaoqingse", Name: "医疗情色"},
	{ID: "kehuanmoshi", Name: "科幻末世"},
	{ID: "renqishunv", Name: "人妻熟女"},
	{ID: "duorenpaidui", Name: "多人派对"},
	{ID: "qingsexiezhen", Name: "情色写真"},
	{ID: "richangyanyu", Name: "日常艳遇"},
	{ID: "yuanchuangduanju", Name: "原创短剧"},
}

func validXjai1Category(category string) bool {
	for _, entry := range nativeXjai1Categories {
		if category == entry.ID {
			return true
		}
	}
	return false
}

func xjai1CatalogPath(category string) string {
	if strings.TrimSpace(category) == "" {
		return "theater"
	}
	return "tags/" + strings.TrimSpace(category)
}

var (
	reXjai1Article = regexp.MustCompile(`(?is)<article\b[^>]*>.*?</article>`)
	reXjai1Drama   = regexp.MustCompile(`(?is)<a\b[^>]*href=["'](/dramas/([^"'/?#]+))["']`)
	reXjai1Strong  = regexp.MustCompile(`(?is)<strong\b[^>]*>(.*?)</strong>`)
	reXjai1Img     = regexp.MustCompile(`(?is)<img\b[^>]*src=["']([^"']+)["']`)
)

func xjai1Client(d *Downloader) *http.Client {
	if d != nil && d.client != nil {
		return d.client
	}
	return http.DefaultClient
}

func (d *Downloader) xjai1Request(ctx context.Context, method, rawURL string, body []byte, bearer string) ([]byte, int, error) {
	client := xjai1Client(d)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		request, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
		if err != nil {
			return nil, 0, err
		}
		request.Header.Set("User-Agent", xjai1UserAgent)
		request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		request.Header.Set("Referer", xjai1BaseURL+"/")
		if body != nil {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json")
			if key := xjai1UUID(); key != "" {
				request.Header.Set("Idempotency-Key", key)
			}
		} else {
			request.Header.Set("Accept", "text/html,application/json,*/*")
		}
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		response, err := client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, providerMaxBodyBytes))
		response.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		return data, response.StatusCode, nil
	}
	if lastErr == nil {
		lastErr = errors.New("香蕉短剧请求失败")
	}
	return nil, 0, lastErr
}

func xjai1UUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// parseXjai1CatalogPage 解析剧场/题材页卡片：只保留不含金币/VIP 字样的卡片
// （与旧 XBPQ 规则的数组过滤语义一致，标题自带“金币”字样的剧同样排除）。
func parseXjai1CatalogPage(body string) ([]Drama, int) {
	blocks := reXjai1Article.FindAllString(body, -1)
	var items []Drama
	for _, block := range blocks {
		if strings.Contains(block, "金币") || strings.Contains(block, "VIP") {
			continue
		}
		href := reXjai1Drama.FindStringSubmatch(block)
		if len(href) < 3 || strings.TrimSpace(href[2]) == "" {
			continue
		}
		title := ""
		if strong := reXjai1Strong.FindStringSubmatch(block); len(strong) >= 2 {
			title = strings.TrimSpace(html.UnescapeString(stripXjai1Tags(strong[1])))
		}
		if title == "" {
			continue
		}
		cover := ""
		if img := reXjai1Img.FindStringSubmatch(block); len(img) >= 2 {
			cover = strings.TrimSpace(img[1])
		}
		episodes := 0
		if match := reEpisodeNumber.FindStringSubmatch(block); len(match) > 0 {
			for _, group := range match[1:] {
				if group == "" {
					continue
				}
				if n, err := strconv.Atoi(group); err == nil {
					episodes = n
					break
				}
			}
		}
		items = append(items, Drama{
			ID:           providerDramaID(sourceXjai1, href[2]),
			Title:        title,
			CoverURL:     cover,
			TotalEpisode: episodes,
		})
	}
	return items, len(blocks)
}

func stripXjai1Tags(raw string) string {
	return regexp.MustCompile(`(?s)<[^>]*>`).ReplaceAllString(raw, "")
}

func (d *Downloader) fetchXjai1CatalogPage(ctx context.Context, page int, category string) ([]Drama, bool, error) {
	if page < 1 {
		page = 1
	}
	address := fmt.Sprintf("%s/%s/page/%d/", xjai1BaseURL, xjai1CatalogPath(category), page)
	body, status, err := d.xjai1Request(ctx, http.MethodGet, address, nil, "")
	if err != nil {
		return nil, false, err
	}
	if status != http.StatusOK {
		return nil, false, fmt.Errorf("香蕉短剧目录请求失败（%d）", status)
	}
	items, rawBlocks := parseXjai1CatalogPage(string(body))
	return items, rawBlocks >= 20, nil
}

type xjai1Episode struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	EpisodeNumber int    `json:"episode_number"`
	Duration      int    `json:"duration_seconds"`
	Commerce      *struct {
		ChargeType string `json:"charge_type"`
		CanPlay    bool   `json:"can_play"`
	} `json:"commerce"`
}

func parseXjai1Episodes(body []byte) ([]xjai1Episode, error) {
	var envelope struct {
		Data []xjai1Episode `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	return envelope.Data, nil
}

func (d *Downloader) fetchXjai1Detail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	detailURL := xjai1BaseURL + "/api/dramas/" + sourceID
	body, status, err := d.xjai1Request(ctx, http.MethodGet, detailURL, nil, "")
	if err != nil {
		return Drama{}, nil, err
	}
	if status != http.StatusOK {
		return Drama{}, nil, fmt.Errorf("香蕉短剧详情请求失败（%d）", status)
	}
	var detail struct {
		Data struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			CoverURL    string `json:"cover_url"`
			EpisodeCnt  int    `json:"episode_count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		return Drama{}, nil, err
	}
	episodesURL := xjai1BaseURL + "/api/dramas/" + sourceID + "/episodes"
	body, status, err = d.xjai1Request(ctx, http.MethodGet, episodesURL, nil, "")
	if err != nil {
		return Drama{}, nil, err
	}
	if status != http.StatusOK {
		return Drama{}, nil, fmt.Errorf("香蕉短剧分集请求失败（%d）", status)
	}
	episodes, err := parseXjai1Episodes(body)
	if err != nil {
		return Drama{}, nil, err
	}
	sort.Slice(episodes, func(i, j int) bool { return episodes[i].EpisodeNumber < episodes[j].EpisodeNumber })
	raw := Drama{
		ID:           providerDramaID(sourceXjai1, sourceID),
		Title:        detail.Data.Title,
		Desc:         detail.Data.Description,
		CoverURL:     detail.Data.CoverURL,
		TotalEpisode: detail.Data.EpisodeCnt,
	}
	var chapters []Chapter
	for _, episode := range episodes {
		if strings.TrimSpace(episode.ID) == "" {
			continue
		}
		if episode.Commerce != nil && !episode.Commerce.CanPlay {
			continue
		}
		number := episode.EpisodeNumber
		if number < 1 {
			number = len(chapters) + 1
		}
		current, _ := json.Marshal(number)
		chapters = append(chapters, Chapter{
			ID:             providerChapterID(sourceXjai1, sourceID, episode.ID),
			Source:         sourceXjai1,
			Title:          fmt.Sprintf("第%d集", number),
			CurrentEpisode: current,
		})
	}
	if len(chapters) == 0 {
		return Drama{}, nil, errors.New("该剧暂时没有可播放的分集")
	}
	if detail.Data.EpisodeCnt <= 0 {
		raw.TotalEpisode = len(chapters)
	}
	return raw, chapters, nil
}

// parseXjai1ChapterID 从 "xjai1:<dramaUUID>:<episodeUUID>" 取出分集 UUID。
func parseXjai1ChapterID(chapterID string) (dramaID, episodeID string, ok bool) {
	parts := strings.Split(strings.TrimSpace(chapterID), ":")
	if len(parts) != 3 || parts[0] != sourceXjai1 || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

type xjai1Session struct {
	PlayMode    string `json:"play_mode"`
	ManifestURL string `json:"manifest_url"`
	Kind        string `json:"kind"`
}

func parseXjai1Session(body []byte) (xjai1Session, error) {
	var envelope struct {
		Data struct {
			PlayMode string `json:"play_mode"`
			Media    struct {
				Kind        string `json:"kind"`
				ManifestURL string `json:"manifest_url"`
			} `json:"media"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return xjai1Session{}, err
	}
	if strings.TrimSpace(envelope.Data.Media.ManifestURL) == "" {
		return xjai1Session{}, errors.New("香蕉短剧未返回播放地址")
	}
	return xjai1Session{
		PlayMode:    envelope.Data.PlayMode,
		Kind:        envelope.Data.Media.Kind,
		ManifestURL: envelope.Data.Media.ManifestURL,
	}, nil
}

// xjai1 访客鉴权（2026-10-21 实测）：
//   首次：POST /api/guest-sessions {"device_id": uuid} → 201，返回 access + refresh；
//   续期：POST /api/auth/refresh {"refresh_token": ...} → 200，返回全新一对；
//   access 约 1 小时、refresh 约 30 天。token 只缓存在 Downloader 内存里，
//   App 重启后重新签发（与浏览器新身份等价），不落盘。

type xjai1TokenPair struct {
	Access  string
	Refresh string
	Expires time.Time // access 的 exp；解析失败时按签发后 50 分钟计
}

func xjai1JWTExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func parseXjai1TokenPair(body []byte) (xjai1TokenPair, error) {
	var envelope struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return xjai1TokenPair{}, err
	}
	if envelope.Data.AccessToken == "" || envelope.Data.RefreshToken == "" {
		return xjai1TokenPair{}, errors.New("香蕉短剧未返回访客 token")
	}
	expires := xjai1JWTExpiry(envelope.Data.AccessToken)
	if expires.IsZero() {
		expires = time.Now().Add(50 * time.Minute)
	}
	return xjai1TokenPair{Access: envelope.Data.AccessToken, Refresh: envelope.Data.RefreshToken, Expires: expires}, nil
}

func (d *Downloader) xjai1MintTokens(ctx context.Context) (xjai1TokenPair, error) {
	if d.xjai1DeviceID == "" {
		d.xjai1DeviceID = xjai1UUID()
	}
	payload, _ := json.Marshal(map[string]string{"device_id": d.xjai1DeviceID})
	body, status, err := d.xjai1Request(ctx, http.MethodPost, xjai1BaseURL+"/api/guest-sessions", payload, "")
	if err != nil {
		return xjai1TokenPair{}, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return xjai1TokenPair{}, fmt.Errorf("香蕉短剧签发访客会话失败（%d）", status)
	}
	return parseXjai1TokenPair(body)
}

func (d *Downloader) xjai1RefreshTokens(ctx context.Context) (xjai1TokenPair, error) {
	payload, _ := json.Marshal(map[string]string{"refresh_token": d.xjai1Refresh})
	body, status, err := d.xjai1Request(ctx, http.MethodPost, xjai1BaseURL+"/api/auth/refresh", payload, "")
	if err != nil {
		return xjai1TokenPair{}, err
	}
	if status != http.StatusOK {
		return xjai1TokenPair{}, fmt.Errorf("香蕉短剧刷新访客 token 失败（%d）", status)
	}
	return parseXjai1TokenPair(body)
}

// xjai1AccessToken 返回可用的 access token；force 为真时无条件续期一次。
func (d *Downloader) xjai1AccessToken(ctx context.Context, force bool) (string, error) {
	d.xjai1Mu.Lock()
	defer d.xjai1Mu.Unlock()
	if d.xjai1Access == "" || d.xjai1Refresh == "" {
		pair, err := d.xjai1MintTokens(ctx)
		if err != nil {
			return "", err
		}
		d.xjai1Access, d.xjai1Refresh, d.xjai1AccessExpires = pair.Access, pair.Refresh, pair.Expires
		return d.xjai1Access, nil
	}
	if force || time.Until(d.xjai1AccessExpires) < 5*time.Minute {
		pair, err := d.xjai1RefreshTokens(ctx)
		if err != nil {
			pair, err = d.xjai1MintTokens(ctx)
			if err != nil {
				return "", err
			}
		}
		d.xjai1Access, d.xjai1Refresh, d.xjai1AccessExpires = pair.Access, pair.Refresh, pair.Expires
	}
	return d.xjai1Access, nil
}

func (d *Downloader) createXjai1Session(ctx context.Context, episodeID string) (xjai1Session, error) {
	payload, _ := json.Marshal(map[string]string{"episode_id": episodeID})
	access, err := d.xjai1AccessToken(ctx, false)
	if err != nil {
		return xjai1Session{}, err
	}
	body, status, err := d.xjai1Request(ctx, http.MethodPost, xjai1BaseURL+"/api/playback/sessions", payload, access)
	if err != nil {
		return xjai1Session{}, err
	}
	if status == http.StatusUnauthorized {
		access, err = d.xjai1AccessToken(ctx, true)
		if err != nil {
			return xjai1Session{}, err
		}
		body, status, err = d.xjai1Request(ctx, http.MethodPost, xjai1BaseURL+"/api/playback/sessions", payload, access)
		if err != nil {
			return xjai1Session{}, err
		}
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return xjai1Session{}, fmt.Errorf("香蕉短剧创建播放会话失败（%d）", status)
	}
	return parseXjai1Session(body)
}

func (d *Downloader) resolveXjai1Media(ctx context.Context, task Task) (providerMedia, error) {
	episodeID := ""
	if _, episode, ok := parseXjai1ChapterID(task.Chapter.ID); ok {
		episodeID = episode
	}
	if episodeID == "" {
		if _, sourceID, valid := splitProviderDramaID(task.DramaID); valid {
			_, chapters, err := d.fetchXjai1Detail(ctx, sourceID)
			if err != nil {
				return providerMedia{}, err
			}
			if task.Index < 0 || task.Index >= len(chapters) {
				return providerMedia{}, errors.New("香蕉短剧分集序号无效")
			}
			if _, episode, ok := parseXjai1ChapterID(chapters[task.Index].ID); ok {
				episodeID = episode
			}
		}
	}
	if episodeID == "" {
		return providerMedia{}, errors.New("香蕉短剧分集信息无效")
	}
	session, err := d.createXjai1Session(ctx, episodeID)
	if err != nil {
		return providerMedia{}, err
	}
	return providerMedia{URL: session.ManifestURL, Referer: xjai1BaseURL + "/"}, nil
}
