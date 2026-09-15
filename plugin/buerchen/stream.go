package buerchen

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"pansou/util/json"
)

type searchEvent struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	IsType      *int   `json:"is_type"`
	IsInvalid   int    `json:"is_invalid"`
	Code        int    `json:"code"`
	Type        string `json:"type"`
}

func (p *BuerchenPlugin) searchProvider(ctx context.Context, client *http.Client, keyword string, provider int) ([]candidate, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := p.cooldownError(); err != nil {
		return nil, err
	}
	query := url.Values{"title": {keyword}, "is_type": {strconv.Itoa(provider)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/other/web_search?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("[%s] 创建搜索请求失败: %w", pluginName, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Referer", p.baseURL+"/s/"+url.PathEscape(keyword)+".html")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[%s] 搜索请求失败: %w", pluginName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, p.apiError("HTTP 搜索", resp.StatusCode)
	}
	// 风控有时直接返回 JSON，而不是 SSE，不能把它当成空搜索。
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
		if err != nil {
			return nil, fmt.Errorf("[%s] 读取搜索响应失败: %w", pluginName, err)
		}
		if len(body) > maxResponseBytes {
			return nil, fmt.Errorf("[%s] 搜索响应超过大小限制", pluginName)
		}
		var status searchEvent
		if json.Unmarshal(body, &status) == nil && status.Code != 0 && status.Code != 200 {
			return nil, p.apiError("搜索", status.Code)
		}
		return nil, fmt.Errorf("[%s] 搜索接口未返回 SSE 数据", pluginName)
	}
	return p.readSearchStream(resp.Body, keyword, provider)
}

func (p *BuerchenPlugin) readSearchStream(reader io.Reader, keyword string, provider int) ([]candidate, error) {
	limited := &io.LimitedReader{R: reader, N: maxResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	entries := make([]candidate, 0, maxCandidatesPerType)
	seen := make(map[string]bool)
	var data []string
	consume := func() (bool, error) {
		payload := strings.TrimSpace(strings.Join(data, "\n"))
		data = data[:0]
		if payload == "" {
			return false, nil
		}
		if strings.HasPrefix(payload, "[DONE]") {
			if strings.Contains(payload, "暂无可用线路") {
				return true, fmt.Errorf("[%s] %s 暂无可用搜索线路", pluginName, providerType(provider))
			}
			return true, nil
		}
		var event searchEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return true, fmt.Errorf("[%s] SSE 数据格式错误", pluginName)
		}
		if event.Code != 0 && event.Code != 200 {
			return true, p.apiError("搜索", event.Code)
		}
		if event.Type == "error" {
			return true, fmt.Errorf("[%s] 搜索流报告线路错误", pluginName)
		}
		event.Title, event.URL = cleanText(event.Title), strings.TrimSpace(event.URL)
		if event.URL == "" || len(event.URL) > 8192 || event.IsInvalid != 0 || seen[event.URL] || !matchesKeyword(event.Title, keyword) {
			return false, nil
		}
		if event.IsType != nil && *event.IsType != provider {
			return false, nil
		}
		seen[event.URL] = true
		entries = append(entries, candidate{title: event.Title, url: event.URL, description: cleanText(event.Description), provider: provider})
		return len(entries) == maxCandidatesPerType, nil
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			if done, err := consume(); done || err != nil {
				return entries, err
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, ":"), strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
			// SSE 注释、心跳和事件元数据不属于资源。
		case strings.HasPrefix(line, "线路："):
			// 站点会在 data 事件之间插入未按 SSE 注释格式发送的线路名称。
		default:
			// 也识别没有正确 Content-Type 的单行风控 JSON。
			var status searchEvent
			if json.Unmarshal([]byte(line), &status) == nil && status.Code != 0 && status.Code != 200 {
				return entries, p.apiError("搜索", status.Code)
			}
			return entries, fmt.Errorf("[%s] 未识别到 SSE 搜索流，可能需要验证或接口已变更", pluginName)
		}
	}
	if limited.N == 0 {
		return entries, fmt.Errorf("[%s] 搜索响应超过大小限制", pluginName)
	}
	if err := scanner.Err(); err != nil {
		return entries, fmt.Errorf("[%s] 读取搜索流失败: %w", pluginName, err)
	}
	if done, err := consume(); done || err != nil {
		return entries, err
	}
	return entries, fmt.Errorf("[%s] 搜索流未正常结束", pluginName)
}
