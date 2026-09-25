package service

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/md5"
	"encoding/gob"
	"encoding/xml"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"pansou/model"
	"pansou/util"
	utiljson "pansou/util/json"
)

const (
	checkStateOK          = "ok"
	checkStateBad         = "bad"
	checkStateLocked      = "locked"
	checkStateUnsupported = "unsupported"
	checkStateUncertain   = "uncertain"
	checkCacheBucketName  = "check_results"
)

type cachedCheckResult struct {
	result    model.CheckResult
	expiresAt time.Time
}

type activeCheckCall struct {
	done   chan struct{}
	result model.CheckResult
	err    error
}

type checkHTTPClientContextKey struct{}

type cachedCheckDiskEntry struct {
	Result    model.CheckResult
	ExpiresAt int64
}

type CheckService struct {
	mu        sync.Mutex
	cache     map[string]cachedCheckResult
	inflight  map[string]*activeCheckCall
	client    *http.Client
	cacheFile string
	cacheDB   *bolt.DB
}

func NewCheckService() *CheckService {
	service := &CheckService{
		cache:     make(map[string]cachedCheckResult),
		inflight:  make(map[string]*activeCheckCall),
		client:    util.GetHTTPClient(),
		cacheFile: filepath.Join(".", "cache", "check_cache.db"),
	}
	service.openCacheStore()
	service.pruneExpiredCacheStore()
	return service
}

func (s *CheckService) Check(items []model.CheckItem) model.CheckResponse {
	response, _ := s.CheckWithProxy(items, "")
	return response
}

func (s *CheckService) CheckWithProxy(items []model.CheckItem, proxyURL string) (model.CheckResponse, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	client := s.client
	cacheScope := ""

	if proxyURL != "" {
		proxyClient, err := util.NewHTTPClient(proxyURL)
		if err != nil {
			return model.CheckResponse{}, err
		}
		defer proxyClient.CloseIdleConnections()

		client = proxyClient
		cacheScope = proxyCacheScope(proxyURL)
	}

	results := make([]model.CheckResult, 0, len(items))

	for _, item := range items {
		results = append(results, s.checkOne(item, client, cacheScope))
	}

	return model.CheckResponse{
		Results: results,
	}, nil
}

func (s *CheckService) checkOne(item model.CheckItem, client *http.Client, cacheScope string) model.CheckResult {
	normalized := s.normalizeShareLink(item.DiskType, item.URL, item.Password)
	if normalized == "" {
		return s.buildResult(item, "", checkStateUncertain, false, "链接格式无效")
	}

	cacheKey := checkCacheKey(item.DiskType, normalized, cacheScope)
	if cached, ok := s.getCached(cacheKey); ok {
		cached.CacheHit = true
		return cached
	}

	call, wait := s.acquireInflight(cacheKey)
	if wait {
		<-call.done
		if call.err != nil {
			return s.buildResult(item, normalized, checkStateUncertain, false, "检测失败")
		}
		result := call.result
		result.CacheHit = false
		return result
	}

	result, err := s.runCheck(item, normalized, client)
	s.finishInflight(cacheKey, call, result, err)

	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "检测失败")
	}

	return result
}

func (s *CheckService) getCached(key string) (model.CheckResult, bool) {
	s.mu.Lock()
	entry, ok := s.cache[key]
	if ok {
		if time.Now().After(entry.expiresAt) {
			delete(s.cache, key)
			s.mu.Unlock()
			s.deletePersistentCache(key)
			return model.CheckResult{}, false
		}

		result := entry.result
		s.mu.Unlock()
		return result, true
	}
	s.mu.Unlock()

	entry, ok = s.loadPersistentCache(key)
	if !ok {
		return model.CheckResult{}, false
	}

	if time.Now().After(entry.expiresAt) {
		s.deletePersistentCache(key)
		return model.CheckResult{}, false
	}

	s.mu.Lock()
	s.cache[key] = entry
	s.mu.Unlock()

	return entry.result, true
}

func (s *CheckService) acquireInflight(key string) (*activeCheckCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if call, ok := s.inflight[key]; ok {
		return call, true
	}

	call := &activeCheckCall{
		done: make(chan struct{}),
	}
	s.inflight[key] = call
	return call, false
}

func (s *CheckService) finishInflight(key string, call *activeCheckCall, result model.CheckResult, err error) {
	var entry cachedCheckResult
	call.result = result
	call.err = err

	s.mu.Lock()
	if err == nil {
		entry = cachedCheckResult{
			result:    result,
			expiresAt: time.UnixMilli(result.ExpiresAt),
		}
		s.cache[key] = entry
	}

	delete(s.inflight, key)
	close(call.done)
	s.mu.Unlock()

	if err == nil {
		s.savePersistentCache(key, entry)
	}
}

func (s *CheckService) runCheck(item model.CheckItem, normalized string, client *http.Client) (model.CheckResult, error) {
	switch item.DiskType {
	case "aliyun":
		return s.checkAliyun(item, normalized, client)
	case "quark":
		return s.checkQuark(item, normalized, client)
	case "uc":
		return s.checkUC(item, normalized, client)
	case "baidu":
		return s.checkBaidu(item, normalized, client)
	case "tianyi":
		return s.checkTianyi(item, normalized, client)
	case "123":
		return s.check123(item, normalized, client)
	case "xunlei":
		return s.checkXunlei(item, normalized, client)
	case "115":
		return s.check115(item, normalized, client)
	case "mobile":
		return s.checkMobile(item, normalized, client)
	default:
		return s.buildResult(item, normalized, checkStateUnsupported, false, "当前平台暂不支持检测"), nil
	}
}

func (s *CheckService) checkAliyun(item model.CheckItem, normalized string, client *http.Client) (model.CheckResult, error) {
	shareID := extractAliyunShareID(normalized)
	if shareID == "" {
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法解析分享地址"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = contextWithCheckHTTPClient(ctx, client)

	body, statusCode, err := s.doJSONRequest(ctx, "POST", "https://api.aliyundrive.com/adrive/v3/share_link/get_share_by_anonymous?share_id="+shareID, map[string]string{
		"share_id": shareID,
	}, map[string]string{
		"content-type": "application/json",
		"origin":       "https://www.alipan.com",
		"referer":      "https://www.alipan.com/",
		"x-canary":     "client=web,app=share,version=v2.3.1",
	})
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求失败"), err
	}

	var parsed struct {
		ShareName   string `json:"share_name"`
		ShareTitle  string `json:"share_title"`
		Code        string `json:"code"`
		Message     string `json:"message"`
		FileCount   *int   `json:"file_count"`
		ShareStatus string `json:"share_status"`
	}
	_ = utiljson.Unmarshal(body, &parsed)

	code := strings.TrimSpace(parsed.Code)
	if code != "" {
		codeLower := strings.ToLower(code)
		message := coalesce(parsed.Message, code)
		switch {
		case strings.Contains(codeLower, "sharelink"):
			return s.buildResult(item, normalized, checkStateBad, false, message), nil
		case containsAny(codeLower, []string{"notfound", "cancelled", "canceled", "forbidden", "expired"}):
			return s.buildResult(item, normalized, checkStateBad, false, message), nil
		case containsAny(codeLower, []string{"exceed", "frequency", "limit"}):
			return s.buildResult(item, normalized, checkStateUncertain, false, message), nil
		default:
			return s.buildResult(item, normalized, checkStateUncertain, false, message), nil
		}
	}

	if parsed.FileCount != nil && *parsed.FileCount == 0 && parsed.ShareName == "" {
		return s.buildResult(item, normalized, checkStateBad, false, "分享内容为空(file_count=0)"), nil
	}

	shareStatus := strings.ToLower(strings.TrimSpace(parsed.ShareStatus))
	if shareStatus != "" && shareStatus != "enabled" && shareStatus != "normal" {
		if containsAny(shareStatus, []string{"forbidden", "cancel", "expired", "illegal", "invalid", "disabled"}) {
			return s.buildResult(item, normalized, checkStateBad, false, coalesce(parsed.Message, "链接失效")), nil
		}
	}

	switch {
	case statusCode == http.StatusOK && (parsed.ShareName != "" || parsed.ShareTitle != "" || (parsed.FileCount != nil && *parsed.FileCount > 0)):
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
	case statusCode != http.StatusOK:
		return s.buildResult(item, normalized, checkStateUncertain, false, coalesce(parsed.Message, fmt.Sprintf("HTTP状态码: %d", statusCode))), nil
	default:
		return s.buildResult(item, normalized, checkStateUncertain, false, parsed.Message), nil
	}
}

func (s *CheckService) checkQuark(item model.CheckItem, normalized string, client *http.Client) (model.CheckResult, error) {
	resourceID, password := extractQuarkShareIDAndPassword(normalized)
	if resourceID == "" {
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法解析分享地址"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = contextWithCheckHTTPClient(ctx, client)

	tokenBody, _, err := s.doJSONRequest(ctx, "POST", "https://drive-h.quark.cn/1/clouddrive/share/sharepage/token", map[string]any{
		"pwd_id":                            resourceID,
		"passcode":                          password,
		"support_visit_limit_private_share": true,
	}, map[string]string{
		"content-type": "application/json",
		"origin":       "https://pan.quark.cn",
		"referer":      "https://pan.quark.cn/",
	})
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求失败"), err
	}

	var tokenResp struct {
		Status  int    `json:"status"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Stoken string `json:"stoken"`
		} `json:"data"`
	}
	_ = utiljson.Unmarshal(tokenBody, &tokenResp)

	switch tokenResp.Code {
	case 0:
	case 41008:
		return s.buildResult(item, normalized, checkStateLocked, false, "需要提取码"), nil
	case 41004, 41010, 41011:
		return s.buildResult(item, normalized, checkStateBad, false, "链接失效"), nil
	default:
		if containsAny(strings.ToLower(tokenResp.Message), []string{"不存在", "失效", "违规", "过期", "取消"}) {
			return s.buildResult(item, normalized, checkStateBad, false, tokenResp.Message), nil
		}
		if containsAny(strings.ToLower(tokenResp.Message), []string{"提取码", "密码"}) {
			return s.buildResult(item, normalized, checkStateLocked, false, tokenResp.Message), nil
		}
		return s.buildResult(item, normalized, checkStateUncertain, false, tokenResp.Message), nil
	}

	if tokenResp.Status != 0 && tokenResp.Status != http.StatusOK {
		return s.buildResult(item, normalized, checkStateBad, false, coalesce(tokenResp.Message, "分享链接失效或不存在")), nil
	}

	if tokenResp.Data.Stoken == "" {
		return s.buildResult(item, normalized, checkStateUncertain, false, "访问令牌缺失"), nil
	}

	detailURL := fmt.Sprintf("https://drive-pc.quark.cn/1/clouddrive/share/sharepage/detail?pwd_id=%s&stoken=%s&ver=2&pr=ucpro", url.QueryEscape(resourceID), url.QueryEscape(tokenResp.Data.Stoken))
	detailBody, _, err := s.doRequest(ctx, "GET", detailURL, nil, map[string]string{
		"accept":        "application/json, text/plain, */*",
		"origin":        "https://pan.quark.cn",
		"referer":       "https://pan.quark.cn/",
		"cache-control": "no-cache",
	})
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "详情请求失败"), err
	}

	var detailResp struct {
		Status  int    `json:"status"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			List  []any `json:"list"`
			Share struct {
				Status           int   `json:"status"`
				PartialViolation bool  `json:"partial_violation"`
				ExpiredAt        int64 `json:"expired_at"`
				ExpiredType      int   `json:"expired_type"`
			} `json:"share"`
			IsExpire bool `json:"is_expire"`
		} `json:"data"`
	}
	_ = utiljson.Unmarshal(detailBody, &detailResp)

	if detailResp.Code != 0 {
		message := coalesce(detailResp.Message, "无法确认链接状态")
		messageLower := strings.ToLower(message)
		switch {
		case containsAny(messageLower, []string{"提取码", "密码", "passcode"}):
			return s.buildResult(item, normalized, checkStateLocked, false, message), nil
		case containsAny(messageLower, []string{"不存在", "失效", "违规", "过期", "取消"}):
			return s.buildResult(item, normalized, checkStateBad, false, message), nil
		default:
			return s.buildResult(item, normalized, checkStateUncertain, false, message), nil
		}
	}

	share := detailResp.Data.Share
	if len(detailResp.Data.List) == 0 {
		switch {
		case share.Status > 1 && share.PartialViolation:
			return s.buildResult(item, normalized, checkStateBad, false, fmt.Sprintf("分享链接部分违规已失效(share_status=%d)", share.Status)), nil
		case share.Status > 1:
			return s.buildResult(item, normalized, checkStateBad, false, fmt.Sprintf("分享链接已失效(share_status=%d)", share.Status)), nil
		case detailResp.Data.IsExpire:
			return s.buildResult(item, normalized, checkStateBad, false, "分享链接已过期"), nil
		default:
			return s.buildResult(item, normalized, checkStateBad, false, "分享链接无效：文件列表为空"), nil
		}
	}

	switch {
	case share.Status == 1 && share.PartialViolation:
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效但部分文件违规"), nil
	case share.Status == 1:
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
	case share.Status == 3 && share.PartialViolation:
		return s.buildResult(item, normalized, checkStateBad, false, "分享链接因违规已失效(share_status=3, partial_violation=true)"), nil
	case share.Status == 3:
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
	case share.Status > 1:
		return s.buildResult(item, normalized, checkStateBad, false, fmt.Sprintf("分享链接已失效(share_status=%d)", share.Status)), nil
	case share.PartialViolation:
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效但部分文件违规"), nil
	default:
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
	}
}

func (s *CheckService) checkUC(item model.CheckItem, normalized string, client *http.Client) (model.CheckResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ctx = contextWithCheckHTTPClient(ctx, client)

	body, statusCode, err := s.doRequest(ctx, "GET", normalized, nil, map[string]string{
		"user-agent": "Mozilla/5.0 (Linux; Android 10; Mobile) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36",
	})
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求失败"), err
	}

	if statusCode == http.StatusNotFound {
		return s.buildResult(item, normalized, checkStateBad, false, "链接失效"), nil
	}

	pageText := strings.ToLower(string(body))
	switch {
	case containsAny(pageText, []string{"失效", "不存在", "违规", "删除", "已过期", "被取消"}):
		return s.buildResult(item, normalized, checkStateBad, false, "链接失效"), nil
	case containsAny(pageText, []string{"提取码", "访问码", "请输入密码"}):
		return s.buildResult(item, normalized, checkStateLocked, false, "需要提取码"), nil
	case containsAny(pageText, []string{"文件", "分享", "drive.uc.cn"}):
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
	default:
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法确认链接状态"), nil
	}
}

func (s *CheckService) checkBaidu(item model.CheckItem, normalized string, client *http.Client) (model.CheckResult, error) {
	shareID, shortURL, password := extractBaiduShareInfo(normalized)
	if shareID == "" || shortURL == "" {
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法解析分享地址"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	ctx = contextWithCheckHTTPClient(ctx, client)

	var bdclnd string
	if password != "" {
		verifyURL := fmt.Sprintf("https://pan.baidu.com/share/verify?surl=%s&pwd=%s", url.QueryEscape(shortURL), url.QueryEscape(password))
		body, _, err := s.doFormRequest(ctx, "POST", verifyURL, url.Values{
			"pwd":       {password},
			"vcode":     {""},
			"vcode_str": {""},
		}, map[string]string{
			"referer":      normalized,
			"content-type": "application/x-www-form-urlencoded",
		})
		if err != nil {
			return s.buildResult(item, normalized, checkStateUncertain, false, "验证失败"), err
		}

		var verifyResp struct {
			Errno  int    `json:"errno"`
			Errmsg string `json:"errmsg"`
			Randsk string `json:"randsk"`
		}
		_ = utiljson.Unmarshal(body, &verifyResp)

		switch verifyResp.Errno {
		case 0:
			bdclnd = verifyResp.Randsk
		case -9, -12:
			return s.buildResult(item, normalized, checkStateLocked, false, "提取码错误或缺失"), nil
		default:
			return s.buildResult(item, normalized, checkStateUncertain, false, verifyResp.Errmsg), nil
		}
	}

	listURL := fmt.Sprintf("https://pan.baidu.com/share/list?web=1&page=1&num=20&order=time&desc=1&showempty=0&shorturl=%s&root=1&clienttype=0", url.QueryEscape(shortURL))
	headers := map[string]string{
		"accept":     "application/json, text/plain, */*",
		"referer":    normalized,
		"user-agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36",
	}
	if bdclnd != "" {
		headers["cookie"] = fmt.Sprintf("BDCLND=%s", bdclnd)
	}

	body, _, err := s.doRequest(ctx, "GET", listURL, nil, headers)
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求失败"), err
	}

	var listResp struct {
		Errno  int    `json:"errno"`
		Errmsg string `json:"errmsg"`
		List   []any  `json:"list"`
	}
	_ = utiljson.Unmarshal(body, &listResp)

	switch listResp.Errno {
	case 0:
		if len(listResp.List) > 0 {
			return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
		}
		return s.buildResult(item, normalized, checkStateBad, false, "链接失效"), nil
	case -9, -12:
		return s.buildResult(item, normalized, checkStateLocked, false, "需要提取码"), nil
	case -7, 105, 115, 117, 145:
		return s.buildResult(item, normalized, checkStateBad, false, "链接失效"), nil
	default:
		return s.buildResult(item, normalized, checkStateUncertain, false, listResp.Errmsg), nil
	}
}

func (s *CheckService) checkTianyi(item model.CheckItem, normalized string, client *http.Client) (model.CheckResult, error) {
	shareCode, password, referer := extractTianyiShareInfo(normalized, item.Password)
	if shareCode == "" {
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法解析分享地址"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = contextWithCheckHTTPClient(ctx, client)

	noCache := fmt.Sprintf("%f", rand.New(rand.NewSource(time.Now().UnixNano())).Float64())
	shareCodeParam := shareCode
	if password != "" {
		shareCodeParam = fmt.Sprintf("%s（访问码：%s）", shareCode, password)
	}

	apiURL := "https://cloud.189.cn/api/open/share/getShareInfoByCodeV2.action"
	targetURL, err := url.Parse(apiURL)
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求地址构造失败"), err
	}

	query := targetURL.Query()
	query.Set("noCache", noCache)
	query.Set("shareCode", shareCodeParam)
	targetURL.RawQuery = query.Encode()

	body, statusCode, err := s.doRequest(ctx, "GET", targetURL.String(), nil, map[string]string{
		"referer":   referer,
		"sign-type": "1",
	})
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求失败"), err
	}

	bodyText := strings.TrimSpace(string(body))

	var shareResponse struct {
		XMLName        xml.Name `xml:"shareVO"`
		NeedAccessCode int      `xml:"needAccessCode"`
		ShareID        int64    `xml:"shareId"`
		FileName       string   `xml:"fileName"`
		AccessCode     string   `xml:"accessCode"`
	}
	if err := xml.Unmarshal(body, &shareResponse); err == nil && shareResponse.XMLName.Local == "shareVO" {
		switch {
		case shareResponse.ShareID > 0:
			return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
		case shareResponse.FileName != "":
			return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
		case shareResponse.NeedAccessCode == 1:
			return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
		}
	}

	var errorResponse struct {
		XMLName xml.Name `xml:"error"`
		Code    string   `xml:"code"`
		Message string   `xml:"message"`
	}
	if err := xml.Unmarshal(body, &errorResponse); err == nil && errorResponse.XMLName.Local == "error" {
		message := mapTianyiErrorMessage(errorResponse.Code, errorResponse.Message)
		messageLower := strings.ToLower(coalesce(errorResponse.Code, errorResponse.Message, message))

		switch {
		case isKnownTianyiErrorCode(errorResponse.Code):
			return s.buildResult(item, normalized, checkStateBad, false, message), nil
		case containsAny(messageLower, []string{"accesscode", "访问码", "提取码", "密码"}):
			return s.buildResult(item, normalized, checkStateLocked, false, message), nil
		case containsAny(messageLower, []string{"shareinfonotfound", "sharenotfound", "filenotfound", "shareexpirederror", "shareauditnotpass", "foldernotfound", "不存在", "失效", "取消", "过期"}):
			return s.buildResult(item, normalized, checkStateBad, false, message), nil
		}
		return s.buildResult(item, normalized, checkStateBad, false, message), nil
	}

	var jsonResponse struct {
		ResCode        int    `json:"res_code"`
		ResMessage     string `json:"res_message"`
		ErrorCode      string `json:"error_code"`
		FileName       string `json:"fileName"`
		NeedAccessCode int    `json:"needAccessCode"`
		ShareID        int64  `json:"shareId"`
	}
	if err := utiljson.Unmarshal(body, &jsonResponse); err == nil {
		if jsonResponse.ErrorCode == "" {
			jsonResponse.ErrorCode = scanTianyiKnownErrorCode(bodyText)
		}

		switch {
		case jsonResponse.ShareID > 0:
			return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
		case jsonResponse.FileName != "":
			return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
		case jsonResponse.NeedAccessCode == 1:
			return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
		case jsonResponse.ErrorCode != "":
			return s.buildResult(item, normalized, checkStateBad, false, mapTianyiErrorMessage(jsonResponse.ErrorCode, jsonResponse.ResMessage)), nil
		case containsAny(strings.ToLower(jsonResponse.ResMessage), []string{"accesscode", "访问码", "提取码", "密码"}):
			return s.buildResult(item, normalized, checkStateLocked, false, coalesce(jsonResponse.ResMessage, "需要访问码")), nil
		}
	}

	if code := scanTianyiKnownErrorCode(bodyText); code != "" {
		return s.buildResult(item, normalized, checkStateBad, false, mapTianyiErrorMessage(code, "")), nil
	}

	switch {
	case statusCode == http.StatusOK && strings.Contains(bodyText, "<shareVO>"):
		if strings.Contains(bodyText, "<shareId>") || strings.Contains(bodyText, "<fileName>") {
			return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
		}
		if strings.Contains(bodyText, "<needAccessCode>1</needAccessCode>") {
			return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
		}
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法确认链接状态"), nil
	case containsAny(strings.ToLower(bodyText), []string{"erroraccesscode", "needaccesscode", "访问码", "提取码", "密码"}):
		return s.buildResult(item, normalized, checkStateLocked, false, "需要访问码"), nil
	case containsAny(strings.ToLower(bodyText), []string{"shareinfonotfound", "sharenotfound", "filenotfound", "shareexpirederror", "shareauditnotpass", "foldernotfound", "不存在", "失效", "取消", "过期"}):
		return s.buildResult(item, normalized, checkStateBad, false, "链接失效"), nil
	default:
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法确认链接状态"), nil
	}
}

func (s *CheckService) check123(item model.CheckItem, normalized string, client *http.Client) (model.CheckResult, error) {
	shareKey := extract123ShareKey(normalized)
	if shareKey == "" {
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法解析分享地址"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	ctx = contextWithCheckHTTPClient(ctx, client)

	apiURL := fmt.Sprintf("https://www.123pan.com/api/share/info?shareKey=%s", url.QueryEscape(shareKey))
	body, statusCode, err := s.doRequest(ctx, "GET", apiURL, nil, nil)
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求失败"), err
	}

	if statusCode == http.StatusForbidden {
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
	}

	var response struct {
		Code int `json:"code"`
		Data struct {
			HasPwd bool `json:"HasPwd"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := utiljson.Unmarshal(body, &response); err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "响应解析失败"), nil
	}

	switch {
	case response.Code == 0:
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
	case response.Data.HasPwd:
		return s.buildResult(item, normalized, checkStateLocked, false, "需要提取码"), nil
	case response.Message != "":
		return s.buildResult(item, normalized, checkStateBad, false, response.Message), nil
	default:
		return s.buildResult(item, normalized, checkStateBad, false, "链接失效"), nil
	}
}

func (s *CheckService) checkXunlei(item model.CheckItem, normalized string, client *http.Client) (model.CheckResult, error) {
	shareID, password := extractXunleiShareInfo(normalized)
	if shareID == "" {
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法解析分享地址"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	ctx = contextWithCheckHTTPClient(ctx, client)

	captchaToken, _ := s.fetchXunleiCaptchaToken(ctx)

	apiURL := fmt.Sprintf("https://api-pan.xunlei.com/drive/v1/share?share_id=%s&pass_code=%s&limit=100&pass_code_token=&page_token=&thumbnail_size=SIZE_SMALL",
		url.QueryEscape(shareID), url.QueryEscape(password))

	headers := map[string]string{
		"accept":          "*/*",
		"content-type":    "application/json",
		"origin":          "https://pan.xunlei.com",
		"referer":         "https://pan.xunlei.com/",
		"user-agent":      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
		"accept-encoding": "gzip, deflate",
		"x-client-id":     "ZUBzD9J_XPXfn7f7",
		"x-device-id":     "5505bd0cab8c9469b98e5891d9fb3e0d",
	}
	if captchaToken != "" {
		headers["x-captcha-token"] = captchaToken
	}

	body, statusCode, err := s.doRequest(ctx, "GET", apiURL, nil, headers)
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求失败"), err
	}

	body, _ = decompressResponseBody(body, headers["accept-encoding"], "")

	if statusCode == http.StatusNotFound || statusCode == http.StatusForbidden {
		return s.buildResult(item, normalized, checkStateBad, false, "链接失效"), nil
	}

	var response struct {
		ErrorCode       int    `json:"error_code"`
		Error           string `json:"error"`
		ErrorMsg        string `json:"error_description"`
		ShareID         string `json:"share_id"`
		PassCode        string `json:"pass_code"`
		FileCount       int    `json:"file_count"`
		ShareName       string `json:"share_name"`
		ShareStatus     string `json:"share_status"`
		ShareStatusText string `json:"share_status_text"`
	}
	if err := utiljson.Unmarshal(body, &response); err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "响应解析失败"), nil
	}

	switch {
	case response.ShareStatus == "OK":
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
	case response.ShareID != "", response.ShareName != "", response.FileCount > 0:
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
	case containsAny(strings.ToLower(response.Error), []string{"pass_code"}), containsAny(strings.ToLower(response.ErrorMsg), []string{"pass_code", "提取码", "密码"}):
		return s.buildResult(item, normalized, checkStateLocked, false, coalesce(response.ErrorMsg, "需要提取码")), nil
	case containsAny(strings.ToLower(response.ShareStatus), []string{"pass_code"}), containsAny(strings.ToLower(response.ShareStatusText), []string{"pass_code", "提取码", "密码"}):
		return s.buildResult(item, normalized, checkStateLocked, false, coalesce(response.ShareStatusText, "需要提取码")), nil
	case response.ShareStatus != "" && response.ShareStatus != "OK":
		summary := coalesce(response.ShareStatusText, fmt.Sprintf("分享状态: %s", response.ShareStatus))
		if containsAny(strings.ToLower(summary), []string{"不存在", "失效", "过期", "not found", "deleted"}) {
			return s.buildResult(item, normalized, checkStateBad, false, summary), nil
		}
		return s.buildResult(item, normalized, checkStateBad, false, summary), nil
	case response.ErrorCode != 0 || response.Error != "" || response.ErrorMsg != "":
		if containsAny(strings.ToLower(response.ErrorMsg), []string{"参数错误", "share_status", "不存在", "失效", "过期", "not found"}) {
			return s.buildResult(item, normalized, checkStateBad, false, coalesce(response.ErrorMsg, "链接失效")), nil
		}
		if containsAny(strings.ToLower(response.Error), []string{"参数错误", "share_status", "不存在", "失效", "过期", "not found"}) {
			return s.buildResult(item, normalized, checkStateBad, false, coalesce(response.ErrorMsg, response.Error)), nil
		}
		if containsAny(strings.ToLower(response.ErrorMsg), []string{"not found", "不存在", "失效", "过期"}) {
			return s.buildResult(item, normalized, checkStateBad, false, coalesce(response.ErrorMsg, "链接失效")), nil
		}
		return s.buildResult(item, normalized, checkStateUncertain, false, coalesce(response.ErrorMsg, response.Error)), nil
	default:
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法确认链接状态"), nil
	}
}

func (s *CheckService) check115(item model.CheckItem, normalized string, client *http.Client) (model.CheckResult, error) {
	shareCode, password := extract115ShareInfo(normalized, item.Password)
	if shareCode == "" {
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法解析分享地址"), nil
	}
	if password == "" {
		return s.buildResult(item, normalized, checkStateLocked, false, "115 需要提取码"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = contextWithCheckHTTPClient(ctx, client)

	apiURL := fmt.Sprintf("https://115cdn.com/webapi/share/snap?share_code=%s&offset=0&limit=20&receive_code=%s&cid=",
		url.QueryEscape(shareCode), url.QueryEscape(password))

	body, _, err := s.doRequest(ctx, "GET", apiURL, nil, map[string]string{
		"priority":           "u=1, i",
		"referer":            fmt.Sprintf("https://115cdn.com/s/%s?password=%s&", shareCode, password),
		"x-requested-with":   "XMLHttpRequest",
		"sec-ch-ua":          `"Chromium";v="142", "Google Chrome";v="142", "Not_A Brand";v="99"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest":     "empty",
		"sec-fetch-mode":     "cors",
		"sec-fetch-site":     "same-origin",
	})
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求失败"), err
	}

	var response struct {
		State bool   `json:"state"`
		Error string `json:"error"`
		Errno int    `json:"errno"`
		Data  struct {
			List       []any `json:"list"`
			Count      int   `json:"count"`
			ShareState int   `json:"share_state"`
			ShareInfo  struct {
				SnapID       string `json:"snap_id"`
				ShareTitle   string `json:"share_title"`
				ShareState   int    `json:"share_state"`
				ForbidReason string `json:"forbid_reason"`
			} `json:"shareinfo"`
		} `json:"data"`
	}
	if err := utiljson.Unmarshal(body, &response); err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "响应解析失败"), nil
	}

	if response.State && response.Errno == 0 {
		shareState := response.Data.ShareState
		if shareState == 0 {
			shareState = response.Data.ShareInfo.ShareState
		}

		reason := strings.TrimSpace(response.Data.ShareInfo.ForbidReason)
		if reason == "" && (shareState == 0 || shareState == 1) {
			if shareState == 1 || len(response.Data.List) > 0 || response.Data.Count > 0 || response.Data.ShareInfo.SnapID != "" || response.Data.ShareInfo.ShareTitle != "" {
				return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
			}
		}

		if reason == "" {
			reason = fmt.Sprintf("链接状态异常(share_state=%d)", shareState)
		}
		if containsAny(strings.ToLower(reason), []string{"密码", "提取码"}) {
			return s.buildResult(item, normalized, checkStateLocked, false, reason), nil
		}
		return s.buildResult(item, normalized, checkStateBad, false, reason), nil
	}

	if containsAny(strings.ToLower(response.Error), []string{"密码", "提取码", "receive_code"}) {
		return s.buildResult(item, normalized, checkStateLocked, false, coalesce(response.Error, "需要提取码")), nil
	}

	if containsAny(strings.ToLower(response.Error), []string{"参数错误", "不存在", "失效", "share_code", "forbid", "forbidden", "违规", "删除", "取消"}) {
		return s.buildResult(item, normalized, checkStateBad, false, coalesce(response.Error, "链接失效")), nil
	}

	if response.Error == "" {
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法确认链接状态"), nil
	}

	return s.buildResult(item, normalized, checkStateBad, false, response.Error), nil
}

func (s *CheckService) checkMobile(item model.CheckItem, normalized string, client *http.Client) (model.CheckResult, error) {
	shareID := extractMobileShareID(normalized)
	if shareID == "" {
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法解析分享地址"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = contextWithCheckHTTPClient(ctx, client)

	requestPayload := map[string]any{
		"getOutLinkInfoReq": map[string]any{
			"account": "",
			"linkID":  shareID,
			"passwd":  item.Password,
			"caSrt":   1,
			"coSrt":   1,
			"srtDr":   0,
			"bNum":    1,
			"pCaID":   "root",
			"eNum":    200,
		},
		"commonAccountInfo": map[string]any{
			"account":     "",
			"accountType": 1,
		},
	}

	encrypted, err := encryptMobilePayload(requestPayload)
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求加密失败"), err
	}

	requestBody, err := utiljson.Marshal(encrypted)
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求序列化失败"), err
	}

	body, _, err := s.doRequest(ctx, "POST", "https://share-kd-njs.yun.139.com/yun-share/richlifeApp/devapp/IOutLink/getOutLinkInfoV6", strings.NewReader(string(requestBody)), map[string]string{
		"accept":        "application/json, text/plain, */*",
		"content-type":  "application/json",
		"hcy-cool-flag": "1",
		"x-deviceinfo":  "||3|12.27.0|chrome|131.0.0.0|5c7c68368f048245e1ce47f1c0f8f2d0||windows 10|1536X695|zh-CN|||",
	})
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "请求失败"), err
	}

	decrypted, err := decryptMobilePayload(string(body))
	if err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "响应解密失败"), nil
	}

	var response map[string]any
	if err := utiljson.Unmarshal([]byte(decrypted), &response); err != nil {
		return s.buildResult(item, normalized, checkStateUncertain, false, "响应解析失败"), nil
	}

	resultCode, _ := response["resultCode"].(string)
	description, _ := response["desc"].(string)
	data := response["data"]

	switch {
	case resultCode == "0" && data != nil:
		return s.buildResult(item, normalized, checkStateOK, false, "链接有效"), nil
	case containsAny(strings.ToLower(description), []string{"提取码", "密码", "访问码"}):
		return s.buildResult(item, normalized, checkStateLocked, false, coalesce(description, "需要提取码")), nil
	case description != "":
		if containsAny(strings.ToLower(description), []string{"失效", "不存在", "过期", "取消"}) {
			return s.buildResult(item, normalized, checkStateBad, false, description), nil
		}
		return s.buildResult(item, normalized, checkStateUncertain, false, description), nil
	case resultCode != "":
		return s.buildResult(item, normalized, checkStateBad, false, "错误码: "+resultCode), nil
	default:
		return s.buildResult(item, normalized, checkStateUncertain, false, "无法确认链接状态"), nil
	}
}

func (s *CheckService) doJSONRequest(ctx context.Context, method, targetURL string, payload any, headers map[string]string) ([]byte, int, error) {
	var reader io.Reader
	if payload != nil {
		raw, err := utiljson.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(raw)
	}

	return s.doRequest(ctx, method, targetURL, reader, headers)
}

func (s *CheckService) doFormRequest(ctx context.Context, method, targetURL string, form url.Values, headers map[string]string) ([]byte, int, error) {
	if headers == nil {
		headers = map[string]string{}
	}
	if _, ok := headers["content-type"]; !ok {
		headers["content-type"] = "application/x-www-form-urlencoded"
	}
	return s.doRequest(ctx, method, targetURL, strings.NewReader(form.Encode()), headers)
}

func (s *CheckService) doRequest(ctx context.Context, method, targetURL string, body io.Reader, headers map[string]string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, targetURL, body)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36")
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	client := checkHTTPClientFromContext(ctx)
	if client == nil {
		client = s.client
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	raw, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return raw, resp.StatusCode, nil
}

func contextWithCheckHTTPClient(ctx context.Context, client *http.Client) context.Context {
	if client == nil {
		return ctx
	}
	return context.WithValue(ctx, checkHTTPClientContextKey{}, client)
}

func checkHTTPClientFromContext(ctx context.Context) *http.Client {
	client, _ := ctx.Value(checkHTTPClientContextKey{}).(*http.Client)
	return client
}

func checkCacheKey(diskType, normalized, cacheScope string) string {
	if cacheScope == "" {
		return diskType + "|" + normalized
	}
	return diskType + "|" + normalized + "|" + cacheScope
}

func proxyCacheScope(proxyURL string) string {
	sum := md5.Sum([]byte(strings.TrimSpace(proxyURL)))
	return fmt.Sprintf("proxy:%x", sum)
}

func (s *CheckService) fetchXunleiCaptchaToken(ctx context.Context) (string, error) {
	deviceID := "5505bd0cab8c9469b98e5891d9fb3e0d"
	clientID := "ZUBzD9J_XPXfn7f7"
	clientVersion := "1.10.0.2633"
	packageName := "com.xunlei.browser"
	timestamp, signature := buildXunleiCaptchaSignature(clientID, clientVersion, packageName, deviceID)

	requestBody := map[string]any{
		"action":        "get:/drive/v1/share",
		"captcha_token": "",
		"client_id":     clientID,
		"device_id":     deviceID,
		"meta": map[string]any{
			"timestamp":      timestamp,
			"captcha_sign":   signature,
			"client_version": clientVersion,
			"package_name":   packageName,
		},
		"redirect_uri": "xlaccsdk01://xunlei.com/callback?state=harbor",
	}

	body, _, err := s.doJSONRequest(ctx, "POST", "https://xluser-ssl.xunlei.com/v1/shield/captcha/init", requestBody, map[string]string{
		"accept":           "application/json;charset=UTF-8",
		"content-type":     "application/json",
		"x-device-id":      deviceID,
		"x-client-id":      clientID,
		"x-client-version": clientVersion,
	})
	if err != nil {
		return "", err
	}

	var response struct {
		CaptchaToken string `json:"captcha_token"`
		URL          string `json:"url"`
	}
	if err := utiljson.Unmarshal(body, &response); err != nil {
		return "", err
	}
	if response.URL != "" {
		return "", fmt.Errorf("xunlei captcha required")
	}
	return response.CaptchaToken, nil
}

func (s *CheckService) buildResult(item model.CheckItem, normalized string, state string, cacheHit bool, summary string) model.CheckResult {
	now := time.Now()
	expiresAt := now.Add(ttlForState(state))

	return model.CheckResult{
		DiskType:      item.DiskType,
		URL:           item.URL,
		NormalizedURL: normalized,
		State:         state,
		CacheHit:      cacheHit,
		CheckedAt:     now.UnixMilli(),
		ExpiresAt:     expiresAt.UnixMilli(),
		Summary:       summary,
	}
}

func (s *CheckService) normalizeShareLink(diskType, rawURL, password string) string {
	base := strings.TrimSpace(rawURL)
	if base == "" {
		return ""
	}

	parsed, err := url.Parse(base)
	if err != nil {
		return base
	}

	parsed.Fragment = ""
	parsed.Host = strings.ToLower(parsed.Host)

	query := parsed.Query()
	if password != "" {
		switch diskType {
		case "baidu", "quark", "uc":
			if query.Get("pwd") == "" {
				query.Set("pwd", password)
			}
		}
	}

	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func ttlForState(state string) time.Duration {
	switch state {
	case checkStateOK:
		return 24 * time.Hour
	case checkStateBad:
		return 6 * time.Hour
	case checkStateLocked:
		return 12 * time.Hour
	case checkStateUnsupported:
		return 24 * time.Hour
	default:
		return 30 * time.Minute
	}
}

func (s *CheckService) openCacheStore() {
	if s.cacheFile == "" {
		return
	}

	if err := os.MkdirAll(filepath.Dir(s.cacheFile), 0o755); err != nil {
		return
	}

	db, err := bolt.Open(s.cacheFile, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return
	}

	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(checkCacheBucketName))
		return err
	}); err != nil {
		_ = db.Close()
		return
	}

	s.cacheDB = db
}

func encodeCachedCheckEntry(entry cachedCheckResult) ([]byte, error) {
	payload := cachedCheckDiskEntry{
		Result:    entry.result,
		ExpiresAt: entry.expiresAt.UnixMilli(),
	}

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(payload); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func decodeCachedCheckEntry(raw []byte) (cachedCheckResult, error) {
	var payload cachedCheckDiskEntry
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&payload); err != nil {
		return cachedCheckResult{}, err
	}

	return cachedCheckResult{
		result:    payload.Result,
		expiresAt: time.UnixMilli(payload.ExpiresAt),
	}, nil
}

func (s *CheckService) loadPersistentCache(key string) (cachedCheckResult, bool) {
	if s.cacheDB == nil {
		return cachedCheckResult{}, false
	}

	var entry cachedCheckResult
	var found bool
	// 读取失败原先被 _ = 吞掉：表现出来只是"缓存未命中"，看起来一切正常，
	// 实际上 DB 损坏/关闭时每次检查都会重读失败，却没有任何线索。
	if err := s.cacheDB.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(checkCacheBucketName))
		if bucket == nil {
			return nil
		}

		raw := bucket.Get([]byte(key))
		if len(raw) == 0 {
			return nil
		}

		decoded, err := decodeCachedCheckEntry(raw)
		if err != nil {
			// 之前这里 return nil，把解码失败也变成了"未命中"；改成向上返回，
			// 让事务把它当作错误交给外层记录——否则损坏的条目会被永久静默忽略。
			return fmt.Errorf("解码检查缓存条目失败: %w", err)
		}

		entry = decoded
		found = true
		return nil
	}); err != nil {
		fmt.Printf("[CHECK] 读取检查缓存失败: %v\n", err)
	}

	return entry, found
}

func (s *CheckService) savePersistentCache(key string, entry cachedCheckResult) {
	if s.cacheDB == nil {
		return
	}

	raw, err := encodeCachedCheckEntry(entry)
	if err != nil {
		return
	}

	_ = s.cacheDB.Batch(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(checkCacheBucketName))
		if bucket == nil {
			return nil
		}
		return bucket.Put([]byte(key), raw)
	})
}

func (s *CheckService) deletePersistentCache(key string) {
	if s.cacheDB == nil {
		return
	}

	_ = s.cacheDB.Batch(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(checkCacheBucketName))
		if bucket == nil {
			return nil
		}
		return bucket.Delete([]byte(key))
	})
}

func (s *CheckService) pruneExpiredCacheStore() {
	if s.cacheDB == nil {
		return
	}

	now := time.Now()
	_ = s.cacheDB.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte(checkCacheBucketName))
		if bucket == nil {
			return nil
		}

		var staleKeys [][]byte
		_ = bucket.ForEach(func(key, value []byte) error {
			entry, err := decodeCachedCheckEntry(value)
			if err != nil || now.After(entry.expiresAt) {
				keyCopy := make([]byte, len(key))
				copy(keyCopy, key)
				staleKeys = append(staleKeys, keyCopy)
			}
			return nil
		})

		for _, key := range staleKeys {
			if err := bucket.Delete(key); err != nil {
				return err
			}
		}

		return nil
	})
}

func buildXunleiCaptchaSignature(clientID, clientVersion, packageName, deviceID string) (string, string) {
	timestamp := fmt.Sprint(time.Now().UnixMilli())
	content := fmt.Sprint(clientID, clientVersion, packageName, deviceID, timestamp)
	parts := []string{
		"uWRwO7gPfdPB/0NfPtfQO+71",
		"F93x+qPluYy6jdgNpq+lwdH1ap6WOM+nfz8/V",
		"0HbpxvpXFsBK5CoTKam",
		"dQhzbhzFRcawnsZqRETT9AuPAJ+wTQso82mRv",
		"SAH98AmLZLRa6DB2u68sGhyiDh15guJpXhBzI",
		"unqfo7Z64Rie9RNHMOB",
		"7yxUdFADp3DOBvXdz0DPuKNVT35wqa5z0DEyEvf",
		"RBG",
		"ThTWPG5eC0UBqlbQ+04nZAptqGCdpv9o55A",
	}

	for _, part := range parts {
		sum := md5.Sum([]byte(content + part))
		content = fmt.Sprintf("%x", sum)
	}

	return timestamp, "1." + content
}

func containsAny(content string, keywords []string) bool {
	for _, keyword := range keywords {
		if strings.Contains(content, strings.ToLower(keyword)) {
			return true
		}
	}
	return false
}

func coalesce(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func mapTianyiErrorMessage(code string, fallback string) string {
	switch strings.TrimSpace(code) {
	case "ShareInfoNotFound":
		return "分享信息不存在"
	case "ShareNotFound":
		return "分享链接不存在"
	case "FileNotFound":
		return "分享文件不存在"
	case "ShareExpiredError":
		return "分享链接已过期"
	case "ShareAuditNotPass":
		return "分享因审核未通过已失效"
	case "FolderNotFound":
		return "分享文件夹不存在"
	default:
		return coalesce(fallback, code)
	}
}

func isKnownTianyiErrorCode(code string) bool {
	return scanTianyiKnownErrorCode(code) != ""
}

func scanTianyiKnownErrorCode(content string) string {
	for _, code := range []string{
		"ShareInfoNotFound",
		"ShareNotFound",
		"FileNotFound",
		"ShareExpiredError",
		"ShareAuditNotPass",
		"FolderNotFound",
	} {
		if strings.Contains(content, code) {
			return code
		}
	}
	return ""
}

func decompressResponseBody(raw []byte, acceptedEncoding string, contentEncoding string) ([]byte, error) {
	encoding := strings.ToLower(contentEncoding)
	if encoding == "" {
		encoding = strings.ToLower(acceptedEncoding)
	}

	if strings.Contains(encoding, "gzip") {
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw, err
		}
		defer reader.Close()
		return io.ReadAll(reader)
	}

	if strings.Contains(encoding, "deflate") {
		reader, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw, err
		}
		defer reader.Close()
		return io.ReadAll(reader)
	}

	return raw, nil
}

func extractAliyunShareID(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	pathParts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(pathParts) == 0 {
		return ""
	}

	return pathParts[len(pathParts)-1]
}

func extractQuarkShareIDAndPassword(rawURL string) (string, string) {
	re := check_serviceRe1
	matches := re.FindStringSubmatch(rawURL)
	if len(matches) < 2 {
		return "", ""
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return matches[1], ""
	}

	return matches[1], parsed.Query().Get("pwd")
}

func extractBaiduShareInfo(rawURL string) (string, string, string) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", "", ""
	}

	queryPwd := parsed.Query().Get("pwd")

	if strings.HasPrefix(parsed.Path, "/s/") {
		shareID := strings.TrimPrefix(parsed.Path, "/s/")
		shortURL := shareID
		if strings.HasPrefix(shortURL, "1") && len(shortURL) > 1 {
			shortURL = shortURL[1:]
		}
		return shareID, shortURL, queryPwd
	}

	if strings.HasPrefix(parsed.Path, "/share/init") {
		shareID := parsed.Query().Get("surl")
		shortURL := shareID
		if strings.HasPrefix(shortURL, "1") && len(shortURL) > 1 {
			shortURL = shortURL[1:]
		}
		return shareID, shortURL, queryPwd
	}

	return "", "", queryPwd
}

func extractTianyiShareInfo(rawURL string, fallbackPassword string) (string, string, string) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fallbackPassword, rawURL
	}

	shareCode := parsed.Query().Get("code")
	if shareCode == "" && strings.HasPrefix(parsed.Path, "/t/") {
		shareCode = strings.TrimPrefix(parsed.Path, "/t/")
	}
	if shareCode == "" && strings.HasPrefix(parsed.Fragment, "/t/") {
		shareCode = strings.TrimPrefix(parsed.Fragment, "/t/")
	}

	if index := strings.Index(shareCode, "/"); index >= 0 {
		shareCode = shareCode[:index]
	}

	password := fallbackPassword
	re := check_serviceRe2
	matches := re.FindStringSubmatch(rawURL)
	if len(matches) >= 2 && matches[1] != "" {
		password = matches[1]
	}

	return shareCode, password, rawURL
}

func extract123ShareKey(rawURL string) string {
	patterns := []string{
		`https?://(?:www\.)?(?:123684|123685|123912|123pan|123592|123865)\.com/s/([a-zA-Z0-9-]+)`,
		`https?://(?:www\.)?123pan\.cn/s/([a-zA-Z0-9-]+)`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(rawURL)
		if len(matches) >= 2 {
			return matches[1]
		}
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 {
		return ""
	}

	return parts[len(parts)-1]
}

func extractXunleiShareInfo(rawURL string) (string, string) {
	re := check_serviceRe3
	matches := re.FindStringSubmatch(rawURL)
	if len(matches) < 2 {
		return "", ""
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return matches[1], ""
	}

	return matches[1], parsed.Query().Get("pwd")
}

func extract115ShareInfo(rawURL string, fallbackPassword string) (string, string) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fallbackPassword
	}

	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) == 0 {
		return "", fallbackPassword
	}

	shareCode := parts[len(parts)-1]
	password := parsed.Query().Get("password")
	if password == "" {
		password = fallbackPassword
	}

	if password == "" && parsed.Fragment != "" && strings.Contains(parsed.Fragment, "password=") {
		if values, err := url.ParseQuery(parsed.Fragment); err == nil {
			password = values.Get("password")
		}
	}

	return shareCode, password
}

func extractMobileShareID(rawURL string) string {
	patterns := []string{
		`https?://(?:www\.)?yun\.139\.com/shareweb/#/w/i/([^&/?#]+)`,
		`https?://(?:www\.)?caiyun\.139\.com/w/i/([^&/?#]+)`,
		`https?://(?:www\.)?caiyun\.139\.com/m/i\?([^&/?#]+)`,
		`https?://caiyun\.feixin\.10086\.cn/([^&/?#]+)`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(rawURL)
		if len(matches) >= 2 {
			return matches[1]
		}
	}

	return ""
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	check_serviceRe1 = regexp.MustCompile(`/s/([A-Za-z0-9]+)`)
	check_serviceRe2 = regexp.MustCompile(`（访问码[：:]\s*([a-zA-Z0-9]+)）`)
	check_serviceRe3 = regexp.MustCompile(`pan\.xunlei\.com/s/([^?/#]+)`)
)
