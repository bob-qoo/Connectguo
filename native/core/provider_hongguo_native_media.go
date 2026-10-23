package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// resolveHongguoAppMedia 取单集 CENC 片源（网页试看范围外的集用这条）。
//
// 2026-10-21 上游修复后的新链：单集端点 /novel/player/video_model/v1/ 在新播放签名
// （Gorgon + Argus + Ladon）下重新可用，payload 精简为 video_id + content_type。
// 这里先走单集端点，失败再回落到 0.2.19 已验证的批量端点
// /novel/player/multi_video_model/preload/v1（一次提交 mixed_video_id_map.1004=[vid]），
// 再失败才交给上层的网页 / 备用取流——三层都不丢。
func (downloader *Downloader) resolveHongguoAppMedia(ctx context.Context, videoID string) (providerMedia, error) {
	if !hongguoNumericID.MatchString(videoID) {
		return providerMedia{}, errors.New("红果视频 ID 无效")
	}
	if media, err := downloader.resolveHongguoSingleVideoMedia(ctx, videoID); err == nil {
		return media, nil
	} else if batchMedia, batchErr := downloader.resolveHongguoBatchVideoMedia(ctx, videoID); batchErr == nil {
		return batchMedia, nil
	} else {
		return providerMedia{}, fmt.Errorf("红果单集取流失败：%v；批量取流失败：%w", publicError(err), batchErr)
	}
}

func (downloader *Downloader) resolveHongguoSingleVideoMedia(ctx context.Context, videoID string) (providerMedia, error) {
	payload := map[string]any{
		"video_id": videoID, "content_type": 1,
		"biz_param": map[string]any{"need_all_video_definition": true, "video_platform": 3},
	}
	result, err := downloader.hongguoAppRequest(ctx, http.MethodPost, "/novel/player/video_model/v1/", nil, payload)
	if err != nil {
		return providerMedia{}, err
	}
	data := nestedMap(result, "data")
	model, _ := data["video_model"].(map[string]any)
	if encoded, ok := data["video_model"].(string); ok && encoded != "" {
		if decoded, err := decodeHongguoBase64(encoded); err == nil && len(decoded) > 0 {
			encoded = string(decoded)
		}
		decoder := json.NewDecoder(strings.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&model); err != nil {
			return providerMedia{}, errors.New("红果 App 播放信息格式异常")
		}
	}
	if len(model) == 0 {
		if rows := anyList(data["video_list"]); len(rows) > 0 {
			model = map[string]any{
				"video_list":     data["video_list"],
				"video_duration": data["video_duration"],
				"duration":       data["duration"],
			}
		}
	}
	if len(model) == 0 {
		return providerMedia{}, errors.New("红果 App 单集取流接口未返回本集片源模型")
	}
	return selectHongguoAppMedia(model)
}

func (downloader *Downloader) resolveHongguoBatchVideoMedia(ctx context.Context, videoID string) (providerMedia, error) {
	payload := map[string]any{
		"biz_param": map[string]any{
			"detail_page_version": 0, "device_level": 3, "disable_digg_stat": false,
			"need_all_video_definition": true, "need_mp4_align": false,
			"use_os_player": false, "use_server_dns": false, "video_platform": 1024,
		},
		"dr_scene":           "preload",
		"mixed_video_id_map": map[string]any{"1004": []string{videoID}},
	}
	result, err := downloader.hongguoAppRequest(ctx, http.MethodPost, "/novel/player/multi_video_model/preload/v1", nil, payload)
	if err != nil {
		return providerMedia{}, err
	}
	data := nestedMap(result, "data")
	if data == nil {
		return providerMedia{}, errors.New("红果 App 批量取流接口未返回数据")
	}
	entry, _ := data[videoID].(map[string]any)
	if len(entry) == 0 {
		// 批量接口的 data 形如 {vid: {...}}；个别情况下外层可能再套一层，兜底取第一个。
		for _, value := range data {
			if item, ok := value.(map[string]any); ok && nestedMap(item, "video_model") != nil {
				entry = item
				break
			}
		}
	}
	model := parseHongguoAppModel(entry)
	if len(model) == 0 {
		return providerMedia{}, errors.New("红果 App 批量取流接口未返回本集片源模型")
	}
	return selectHongguoAppMedia(model)
}

// parseHongguoAppModel 把批量端点 data[vid].video_model 还原成模型 map。
// video_model 通常是内嵌的 JSON 字符串；个别实现可能直接是对象。两种都兼容。
func parseHongguoAppModel(entry map[string]any) map[string]any {
	if entry == nil {
		return nil
	}
	if model, ok := entry["video_model"].(map[string]any); ok {
		return model
	}
	encoded, ok := entry["video_model"].(string)
	if !ok {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.UseNumber()
	var model map[string]any
	if err := decoder.Decode(&model); err != nil {
		return nil
	}
	return model
}

func selectHongguoAppMedia(model map[string]any) (providerMedia, error) {
	variants := anyList(model["video_list"])
	if rows, ok := model["video_list"].(map[string]any); ok && len(variants) == 0 {
		keys := make([]string, 0, len(rows))
		for key := range rows {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			variants = append(variants, rows[key])
		}
	}
	duration, _ := strconv.ParseFloat(mapString(model, "video_duration", "duration"), 64)
	var keyErr error
	type scoredMedia struct {
		media providerMedia
		score int
	}
	var choices []scoredMedia
	for _, row := range variants {
		variant, _ := row.(map[string]any)
		meta := nestedMap(variant, "video_meta")
		codec := strings.ToLower(mapString(meta, "codec_type"))
		if codec == "bytevc2" || strings.Contains(strings.ToLower(mapString(variant, "gear_des_key")), "bytevc2") {
			continue
		}
		addresses := hongguoMediaAddresses(variant)
		if len(addresses) == 0 {
			continue
		}
		media := providerMedia{Referer: "https://novel.snssdk.com/", Duration: time.Duration(duration * float64(time.Second))}
		encryption := nestedMap(variant, "encrypt_info")
		spade := mapString(encryption, "spade_a")
		if spade != "" || encryption["encrypt"] == true || mapString(encryption, "encryption_method") == "cenc-aes-ctr" {
			var err error
			media.CENCKey, err = hongguoContentKey(spade)
			if err != nil {
				keyErr = err
				continue
			}
		}
		height, _ := strconv.Atoi(mapString(meta, "vheight"))
		if definition, err := strconv.Atoi(hongguoQualityNumber.FindString(mapString(meta, "definition"))); err == nil && definition > 0 {
			height = definition
		} else if width, _ := strconv.Atoi(mapString(meta, "vwidth")); width > 0 && (height == 0 || width < height) {
			height = width
		}
		media.Quality = height
		quality := height * 10
		if codec == "h264" || codec == "avc1" {
			quality++
		}
		for _, address := range addresses {
			media.URL = address
			choices = append(choices, scoredMedia{media: media, score: quality})
		}
	}
	if len(choices) > 0 {
		sort.SliceStable(choices, func(i, j int) bool { return choices[i].score > choices[j].score })
		selected := choices[0].media
		for _, choice := range choices {
			selected.Variants = append(selected.Variants, choice.media)
		}
		return selected, nil
	}
	if keyErr != nil {
		return providerMedia{}, fmt.Errorf("红果 App 媒体密钥不可用: %w", keyErr)
	}
	return providerMedia{}, errors.New("红果 App 未返回兼容的媒体，已跳过不支持的编码")
}

func hongguoMediaAddresses(info map[string]any) []string {
	var addresses []string
	seen := map[string]bool{}
	var add func(any)
	add = func(value any) {
		switch value := value.(type) {
		case string:
			address := strings.TrimSpace(value)
			if len(address) > 8192 {
				return
			}
			if !isProviderHTTPMediaURL(address) {
				decoded, err := decodeHongguoBase64(address)
				if err != nil {
					return
				}
				address = strings.TrimSpace(string(decoded))
			}
			if isProviderHTTPMediaURL(address) && !seen[address] {
				seen[address] = true
				addresses = append(addresses, address)
			}
		case []any:
			for _, item := range value {
				add(item)
			}
		}
	}
	for _, key := range []string{"main_url", "backup_url", "backup_url_1", "backup_url_2", "backup_urls", "url_list"} {
		add(info[key])
	}
	return addresses
}
