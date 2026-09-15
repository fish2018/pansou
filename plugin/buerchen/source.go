package buerchen

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"pansou/model"
	"pansou/util/json"
)

const requestTimeout = 12 * time.Second

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

var (
	sharePathPattern = regexp.MustCompile(`^/s/[A-Za-z0-9_-]+/?$`)
	passwordValue    = regexp.MustCompile(`^[A-Za-z0-9]{4,8}$`)
	passwordText     = regexp.MustCompile(`(?i)(?:提取码|访问码|密码|pwd|password)\s*[:：=]?\s*([a-z0-9]{4,8})(?:[^a-z0-9]|$)`)
)

func normalizeShareLink(raw, passcode string) (model.Link, bool) {
	fields := strings.Fields(html.UnescapeString(strings.TrimSpace(raw)))
	if len(fields) == 0 {
		return model.Link{}, false
	}
	u, err := url.Parse(fields[0])
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" || !sharePathPattern.MatchString(u.Path) {
		return model.Link{}, false
	}
	var linkType string
	switch strings.ToLower(u.Hostname()) {
	case "pan.quark.cn":
		linkType = "quark"
	case "pan.baidu.com":
		linkType = "baidu"
	case "pan.xunlei.com":
		linkType = "xunlei"
	default:
		return model.Link{}, false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return model.Link{}, false
	}
	password := ""
	for _, key := range []string{"pwd", "password"} {
		if value := query.Get(key); passwordValue.MatchString(value) {
			password = value
			break
		}
	}
	if password == "" {
		if passwordValue.MatchString(passcode) {
			password = passcode
		} else if match := passwordText.FindStringSubmatch(strings.Join(fields[1:], " ") + " " + passcode); len(match) > 1 {
			password = match[1]
		}
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	u.RawQuery = query.Encode()
	u.ForceQuery = false
	return model.Link{Type: linkType, URL: u.String(), Password: password}, true
}

func shareIdentity(link model.Link) string {
	u, _ := url.Parse(link.URL)
	return link.Type + "\x00" + u.Path
}

func (p *BuerchenPlugin) requestJSON(ctx context.Context, client *http.Client, method, path string, payload, target interface{}) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if err := p.cooldownError(); err != nil {
		return err
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("[%s] 编码请求失败: %w", pluginName, err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("[%s] 创建请求失败: %w", pluginName, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.Header.Set("Referer", p.baseURL+"/")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", p.baseURL)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("[%s] 请求接口失败: %w", pluginName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return p.apiError("HTTP 请求", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("[%s] 读取响应失败: %w", pluginName, err)
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("[%s] 响应超过大小限制", pluginName)
	}
	if err := json.Unmarshal(data, target); err != nil {
		// 不把响应正文写入错误，转存接口可能返回网盘内部字段。
		return fmt.Errorf("[%s] 响应不是有效的 JSON，可能需要验证或接口已变更", pluginName)
	}
	return nil
}

func (p *BuerchenPlugin) apiError(operation string, code int) error {
	err := fmt.Errorf("[%s] %s失败 (code=%d)", pluginName, operation, code)
	switch code {
	case http.StatusForbidden, 42901:
		err = fmt.Errorf("[%s] 站点要求安全验证 (CHALLENGE_REQUIRED, code=%d)，暂停请求 1 分钟", pluginName, code)
	case http.StatusTooManyRequests:
		err = fmt.Errorf("[%s] 站点限流 (HTTP 429)，暂停请求 1 分钟", pluginName)
	default:
		return err
	}
	p.mu.Lock()
	p.blockedUntil, p.blockedErr = time.Now().Add(requestCooldown), err
	p.mu.Unlock()
	return err
}

func (p *BuerchenPlugin) cooldownError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.blockedUntil) {
		return p.blockedErr
	}
	return nil
}

func (p *BuerchenPlugin) getCachedShare(key string) (resolvedShare, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.shareCache[key]
	if !ok || time.Now().After(entry.expiresAt) {
		delete(p.shareCache, key)
		return resolvedShare{}, false
	}
	return entry.share, true
}

func (p *BuerchenPlugin) putCachedShare(key string, share resolvedShare) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.shareCache[key]; !exists && len(p.shareCache) >= maxCacheEntries {
		oldestKey := ""
		var oldestTime time.Time
		for k, entry := range p.shareCache {
			if oldestKey == "" || entry.expiresAt.Before(oldestTime) {
				oldestKey, oldestTime = k, entry.expiresAt
			}
		}
		delete(p.shareCache, oldestKey)
	}
	p.shareCache[key] = cachedShare{share: share, expiresAt: time.Now().Add(shareCacheTTL)}
}
