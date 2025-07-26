package panyq

import (
	"crypto/tls"
	"pansou/util/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"net/http/cookiejar"

	"pansou/model"
	"pansou/plugin"
)

// 常量定义
const (
	// 默认超时时间
	DefaultTimeout = 15 * time.Second
	// 最大并发数
	MaxConcurrency = 100
	// 默认请求重试次数
	MaxRetries = 0 // 重试
	// 是否开启调试日志
	DebugLog = false // 修改为true，使得调试信息可见
	// 配置文件名
	ConfigFileName = "panyq_config.json"
	// 基础URL
	BaseURL = "https://panyq.com"
	// 请求来源控制默认为开启状态
	EnableRefererCheck = true
)

// 动态Action ID的键名
var ActionIDKeys = []string{
	"credential_action_id",     // 获取凭证用的ID
	"intermediate_action_id",   // 中间步骤用的ID
	"final_link_action_id",     // 获取最终链接用的ID
}

// 凭证结构
type Credentials struct {
	Sign string `json:"sign"`
	Hash string `json:"hash"`
	Sha  string `json:"sha"`
}

// 搜索结果项目
type SearchHit struct {
	EID     string `json:"eid"`
	Desc    string `json:"desc"`
	SizeStr string `json:"size_str"`
}

// 搜索响应
type SearchResponse struct {
	Data struct {
		Hits       []SearchHit `json:"hits"`
		MaxPageNum int         `json:"maxPageNum"`
	} `json:"data"`
}

// 配置缓存，用于在多个搜索过程中复用Action ID
var (
	actionIDCache     = make(map[string]string)
	actionIDCacheLock sync.RWMutex
	
	// 允许的请求来源列表，可以直接修改这个变量来控制 ext={"referer":"xxx"}
	AllowedReferers = []string{
		"https://dm.xueximeng.com",
		"http://localhost",
		// 可以添加更多允许的来源
	}
	
	// 新增缓存
	// 最终链接缓存
	finalLinkCache     = make(map[string]string)
	finalLinkCacheLock sync.RWMutex
	
	// 搜索结果缓存
	searchResultCache     = make(map[string][]model.SearchResult)
	searchResultCacheLock sync.RWMutex
)

// PanyqPlugin 盘友圈搜索插件
type PanyqPlugin struct {
	*plugin.BaseAsyncPlugin
	client *http.Client
}

// NewPanyqPlugin 创建新的盘友圈搜索插件
func NewPanyqPlugin() *PanyqPlugin {
	// 创建一个可以忽略HTTPS证书验证并支持Cookie的HTTP客户端
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		// 启用HTTP/2
		ForceAttemptHTTP2: true,
		// 启用连接复用
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	}
	
	client := &http.Client{
		Timeout:   DefaultTimeout,
		Transport: transport,
		Jar:       jar, // 使用Cookie管理
		// 自动处理重定向
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
	}

	return &PanyqPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("panyq", 2),
		client:          client,
	}
}

// Search 执行搜索并返回结果
func (p *PanyqPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if DebugLog {
		fmt.Println("panyq: ext 参数内容:", ext)
	}

	// 检查搜索结果缓存
	cacheKey := fmt.Sprintf("search:%s", keyword)
	searchResultCacheLock.RLock()
	if cachedResults, ok := searchResultCache[cacheKey]; ok {
		searchResultCacheLock.RUnlock()
		if DebugLog {
			fmt.Printf("panyq: 缓存命中搜索结果: %s\n", keyword)
		}
		return cachedResults, nil
	}
	searchResultCacheLock.RUnlock()

	// 请求来源检查
	if EnableRefererCheck && ext != nil {
		referer := ""
		if refererVal, ok := ext["referer"].(string); ok {
			referer = refererVal
		}
		
		// 检查referer是否在允许列表中
		allowed := false
		for _, allowedReferer := range AllowedReferers {
			if strings.HasPrefix(referer, allowedReferer) {
				if DebugLog {
					fmt.Printf("panyq: 允许来自 %s 的请求\n", referer)
				}
				allowed = true
				break
			}
		}
		
		if !allowed {
			if DebugLog {
				fmt.Printf("panyq: 拒绝来自 %s 的请求\n", referer)
			}
			return nil, fmt.Errorf("请求来源不被允许")
		}
	}
	
	// 使用基础异步插件的搜索方法
	results, err := p.AsyncSearch(keyword, p.doSearch, p.MainCacheKey, ext)
	
	// 如果搜索成功，缓存结果
	if err == nil && len(results) > 0 {
		searchResultCacheLock.Lock()
		searchResultCache[cacheKey] = results
		searchResultCacheLock.Unlock()
	}
	
	return results, err
}

// doSearch 实际的搜索实现
func (p *PanyqPlugin) doSearch(client *http.Client, keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	if DebugLog {
		fmt.Println("panyq: searching for", keyword)
	}

	// 尝试获取或发现 Action ID
	actionIDs, err := p.getOrDiscoverActionIDs()
	if err != nil {
		fmt.Println("panyq: failed to get Action IDs:", err)
		return nil, fmt.Errorf("获取Action ID失败: %w", err)
	}

	// 步骤1: 获取搜索凭证
	credentials, err := p.getCredentials(keyword, actionIDs[ActionIDKeys[0]], client)
	if err != nil {
		// 如果获取凭证失败，尝试刷新Action ID并重试
		fmt.Println("panyq: failed to get credentials, refreshing Action IDs...")
		actionIDs, err = p.discoverActionIDs()
		if err != nil {
			return nil, fmt.Errorf("刷新Action ID失败: %w", err)
		}
		
		// 使用新的Action ID重试获取凭证
		credentials, err = p.getCredentials(keyword, actionIDs[ActionIDKeys[0]], client)
		if err != nil {
			return nil, fmt.Errorf("获取搜索凭证失败: %w", err)
		}
	}

	// 步骤2: 获取第一页搜索结果列表
	hits, maxPageNum, err := p.getSearchResults(credentials.Sign, 1, client)
	if err != nil {
		return nil, fmt.Errorf("获取搜索结果失败: %w", err)
	}

	if len(hits) == 0 {
		if DebugLog {
			fmt.Println("panyq: no results found for", keyword)
		}
		return []model.SearchResult{}, nil
	}
	
	// 如果有多页结果，并发获取其他页的数据
	if maxPageNum > 1 {
		if DebugLog {
			fmt.Printf("panyq: found %d pages, fetching additional pages...\n", maxPageNum)
		}
		if maxPageNum >= 3 {
			maxPageNum = 3
		}
		// 创建通道存储其他页的结果
		hitsChan := make(chan []SearchHit, maxPageNum-1)
		var wg sync.WaitGroup
		
		// 并发获取第2页到最后一页
		for page := 2; page <= maxPageNum; page++ {
			wg.Add(1)
			go func(pageNum int) {
				defer wg.Done()
				
				if DebugLog {
					fmt.Printf("panyq: fetching page %d...\n", pageNum)
				}
				
				pageHits, _, err := p.getSearchResults(credentials.Sign, pageNum, client)
				if err != nil {
					fmt.Printf("panyq: failed to get page %d: %v\n", pageNum, err)
					return
				}
				
				hitsChan <- pageHits
			}(page)
		}
		
		// 等待所有页面获取完成
		go func() {
			wg.Wait()
			close(hitsChan)
		}()
		
		// 合并所有页面的结果
		for pageHits := range hitsChan {
			hits = append(hits, pageHits...)
		}
		
		if DebugLog {
			fmt.Printf("panyq: total %d results from all pages\n", len(hits))
		}
	}

	// 使用并发控制通道限制并发数
	sem := make(chan struct{}, MaxConcurrency)
	var wg sync.WaitGroup
	
	// 创建结果和错误通道
	resultChan := make(chan model.SearchResult, len(hits))
	
	// 并发处理每个搜索结果
	for i, hit := range hits {
		wg.Add(1)
		sem <- struct{}{} // 获取信号量
		
		go func(index int, item SearchHit) {
			defer wg.Done()
			defer func() { <-sem }() // 释放信号量
			
			// 步骤3: 执行中间状态确认
			err := p.performIntermediateStep(actionIDs[ActionIDKeys[1]], credentials.Hash, credentials.Sha, item.EID, client)
			if err != nil {
				fmt.Println("panyq: intermediate step failed for", item.EID, ":", err)
				return
			}
			
			// 步骤4: 获取最终链接
			finalLink, err := p.getFinalLink(actionIDs[ActionIDKeys[2]], item.EID, client)
			if err != nil {
				fmt.Println("panyq: get final link failed for", item.EID, ":", err)
				return
			}
			
			if finalLink == "" {
				return
			}
			
			// 创建链接
			linkType := p.determineLinkType(finalLink)
			links := []model.Link{
				{
					URL:      finalLink,
					Type:     linkType,
					Password: p.extractPassword(finalLink, linkType),
				},
			}
			
			// 清理标题和内容中的HTML标签
			title := p.extractTitle(item.Desc)
			cleanedDesc := p.cleanEscapedHTML(item.Desc)
			
			// 创建搜索结果
			result := model.SearchResult{
				UniqueID:  fmt.Sprintf("panyq-%d", index),
				Title:     title,
				Content:   cleanedDesc,
				Links:     links,
				Datetime:  time.Time{}, // 没有时间信息，使用零值
			}
			
			resultChan <- result
		}(i, hit)
	}
	
	// 启动协程等待所有任务完成并关闭通道
	go func() {
		wg.Wait()
		close(resultChan)
	}()
	
	// 收集结果
	var results []model.SearchResult
	for result := range resultChan {
		// 确保result中的标题和内容已清理HTML标签
		result.Title = p.cleanEscapedHTML(result.Title)
		result.Content = p.cleanEscapedHTML(result.Content)
		results = append(results, result)
	}

	// 使用关键词过滤结果
	filteredResults := p.FilterResultsByKeyword(results, keyword)

	if DebugLog {
		fmt.Println("panyq: returning", len(filteredResults), "filtered results")
	}

	return filteredResults, nil
}

// getOrDiscoverActionIDs 获取或发现Action ID
func (p *PanyqPlugin) getOrDiscoverActionIDs() (map[string]string, error) {
	// 先检查缓存
	actionIDCacheLock.RLock()
	if len(actionIDCache) >= len(ActionIDKeys) {
		ids := make(map[string]string)
		for _, key := range ActionIDKeys {
			if id, ok := actionIDCache[key]; ok {
				ids[key] = id
			}
		}
		
		if len(ids) == len(ActionIDKeys) {
			actionIDCacheLock.RUnlock()
			return ids, nil
		}
	}
	actionIDCacheLock.RUnlock()
	
	// 没有缓存或缓存不完整，发现新的Action ID
	return p.discoverActionIDs()
}

// discoverActionIDs 发现Action ID
func (p *PanyqPlugin) discoverActionIDs() (map[string]string, error) {
	if DebugLog {
		fmt.Println("panyq: discovering Action IDs...")
	}
	
	// 尝试从缓存文件加载
	finalIDs, err := p.loadActionIDsFromFile()
	if err == nil && len(finalIDs) == len(ActionIDKeys) {
		if DebugLog {
			fmt.Println("panyq: loaded Action IDs from file cache")
		}
		
		// 保存到内存缓存
		actionIDCacheLock.Lock()
		for k, v := range finalIDs {
			actionIDCache[k] = v
		}
		actionIDCacheLock.Unlock()
		
		return finalIDs, nil
	}
	
	// 从网站获取潜在的Action ID
	potentialIDs, err := p.findPotentialActionIDs(p.client)
	if err != nil {
		return nil, err
	}
	
	if len(potentialIDs) == 0 {
		return nil, fmt.Errorf("未找到潜在的Action ID")
	}
	
	if DebugLog {
		fmt.Printf("panyq: 找到 %d 个潜在的 Action ID\n", len(potentialIDs))
		if len(potentialIDs) > 0 {
			fmt.Printf("panyq: 样例ID: %s\n", potentialIDs[0])
		}
	}
	
	finalIDs = make(map[string]string)
	
	// 1. 验证credential_action_id - 并发验证
	if DebugLog {
		fmt.Println("panyq: validating credential_action_id...")
	}
	
	// 使用通道存储验证成功的ID
	credIDChan := make(chan string, len(potentialIDs))
	
	// 并发验证所有ID
	var wg sync.WaitGroup
	for i, id := range potentialIDs {
		wg.Add(1)
		go func(index int, actionID string) {
			defer wg.Done()
			if DebugLog {
				fmt.Printf("panyq: 并发尝试第 %d 个ID作为credential_action_id: %.10s...\n", index+1, actionID)
			}
			if p.validateCredentialID(actionID) {
				if DebugLog {
					fmt.Printf("panyq: 找到有效的credential_action_id: %s\n", actionID)
				}
				credIDChan <- actionID
			}
		}(i, id)
	}
	
	// 等待所有验证完成
	wg.Wait()
	close(credIDChan)
	
	// 从通道中获取第一个有效ID
	var credentialIDFound bool
	for id := range credIDChan {
		finalIDs[ActionIDKeys[0]] = id
		credentialIDFound = true
		break
	}
	
	if !credentialIDFound {
		return nil, fmt.Errorf("未能验证credential_action_id")
	}
	
	// 获取测试凭证用于后续验证
	testCreds, err := p.getCredentials("test", finalIDs[ActionIDKeys[0]], p.client)
	if err != nil {
		return nil, fmt.Errorf("获取测试凭证失败: %w", err)
	}
	
	if DebugLog {
		fmt.Printf("panyq: 获取到测试凭证: sign=%.10s..., hash=%.10s..., sha=%.10s...\n", 
			testCreds.Sign, testCreds.Hash, testCreds.Sha)
	}
	
	// 从剩余ID中排除已使用的ID
	remainingIDs := make([]string, 0, len(potentialIDs)-1)
	for _, id := range potentialIDs {
		if id != finalIDs[ActionIDKeys[0]] {
			remainingIDs = append(remainingIDs, id)
		}
	}
	
	// 2. 验证intermediate_action_id - 从后向前验证
	if DebugLog {
		fmt.Printf("panyq: validating intermediate_action_id (%d candidates)...\n", len(remainingIDs))
	}
	
	var intermediateIDFound bool
	
	// 从后向前验证
	for i := len(remainingIDs) - 1; i >= 0; i-- {
		id := remainingIDs[i]
		if DebugLog {
			fmt.Printf("panyq: 尝试第 %d 个剩余ID作为intermediate_action_id: %.10s...\n", i+1, id)
		}
		if p.validateIntermediateID(id, testCreds.Hash, testCreds.Sha) {
			finalIDs[ActionIDKeys[1]] = id
			intermediateIDFound = true
			if DebugLog {
				fmt.Printf("panyq: 找到有效的intermediate_action_id: %s\n", id)
			}
			break
		}
	}
	
	if !intermediateIDFound {
		return nil, fmt.Errorf("未能验证intermediate_action_id")
	}
	
	// 获取测试EID
	testHits, _, err := p.getSearchResults(testCreds.Sign, 1, p.client) // 获取第一页测试结果
	if err != nil {
		return nil, fmt.Errorf("获取测试结果失败: %w", err)
	}
	
	if len(testHits) == 0 {
		return nil, fmt.Errorf("获取测试EID失败: 无搜索结果")
	}
	
	testEID := testHits[0].EID
	
	if DebugLog {
		fmt.Printf("panyq: 获取到测试EID: %s\n", testEID)
	}
	
	// 从剩余ID中排除已使用的ID
	newRemainingIDs := make([]string, 0, len(remainingIDs)-1)
	for _, id := range remainingIDs {
		if id != finalIDs[ActionIDKeys[1]] {
			newRemainingIDs = append(newRemainingIDs, id)
		}
	}
	remainingIDs = newRemainingIDs
	
	// 3. 验证final_link_action_id
	if DebugLog {
		fmt.Printf("panyq: validating final_link_action_id (%d candidates)...\n", len(remainingIDs))
	}
	
	var finalLinkIDFound bool
	for i, id := range remainingIDs {
		// 针对每个候选ID都执行一次中间步骤
		if DebugLog {
			fmt.Printf("panyq: 尝试第 %d 个ID作为final_link_action_id: %.10s...\n", i+1, id)
			fmt.Println("panyq: 执行中间步骤...")
		}
		
		err = p.performIntermediateStep(finalIDs[ActionIDKeys[1]], testCreds.Hash, testCreds.Sha, testEID, p.client)
		if err != nil {
			fmt.Printf("panyq: 中间步骤执行失败, 继续尝试下一个ID: %v\n", err)
			continue
		}
		
		if DebugLog {
			fmt.Println("panyq: 验证final_link_action_id...")
		}
		
		if p.validateFinalLinkID(id, testEID) {
			finalIDs[ActionIDKeys[2]] = id
			finalLinkIDFound = true
			if DebugLog {
				fmt.Printf("panyq: 找到有效的final_link_action_id: %s\n", id)
			}
			break
		}
	}
	
	if !finalLinkIDFound {
		// 如果只剩下一个ID且验证失败，尝试交换intermediate_action_id和final_link_action_id
		if len(remainingIDs) == 1 && len(potentialIDs) == 3 {
			if DebugLog {
				fmt.Println("panyq: final_link_action_id验证失败，尝试交换intermediate_action_id和final_link_action_id...")
			}
			
			// 保存当前的intermediate_action_id
			oldInterID := finalIDs[ActionIDKeys[1]]
			
			// 使用剩余的ID作为intermediate_action_id
			finalIDs[ActionIDKeys[1]] = remainingIDs[0]
			
			// 使用原来的intermediate_action_id作为final_link_action_id
			finalIDs[ActionIDKeys[2]] = oldInterID
			
			// 执行中间步骤
			err = p.performIntermediateStep(finalIDs[ActionIDKeys[1]], testCreds.Hash, testCreds.Sha, testEID, p.client)
			if err != nil {
				if DebugLog {
					fmt.Printf("panyq: 交换后中间步骤执行失败: %v\n", err)
				}
			} else {
				// 验证final_link_action_id
				if p.validateFinalLinkID(finalIDs[ActionIDKeys[2]], testEID) {
					finalLinkIDFound = true
					if DebugLog {
						fmt.Println("panyq: 交换ID后验证成功!")
					}
				}
			}
		}
		
		if !finalLinkIDFound {
			return nil, fmt.Errorf("未能验证final_link_action_id")
		}
	}
	
	// 保存到内存缓存
	actionIDCacheLock.Lock()
	for k, v := range finalIDs {
		actionIDCache[k] = v
	}
	actionIDCacheLock.Unlock()
	
	// 保存到文件缓存
	if err := p.saveActionIDsToFile(finalIDs); err != nil {
		fmt.Printf("panyq: 保存Action IDs到文件失败: %v\n", err)
		// 继续执行，不返回错误
	}
	
	if DebugLog {
		fmt.Println("panyq: all Action IDs validated successfully:")
		for _, key := range ActionIDKeys {
			fmt.Printf("panyq:   %s = %s\n", key, finalIDs[key])
		}
	}
	
	return finalIDs, nil
}

// findPotentialActionIDs 从网站获取潜在的Action ID
func (p *PanyqPlugin) findPotentialActionIDs(client *http.Client) ([]string, error) {
	// 请求网站首页
	req, err := http.NewRequest("GET", BaseURL, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	
	// 只保留指定的请求头
	// req.Header.Set("sec-ch-ua", `"Not)A;Brand";v="8", "Chromium";v="138", "Google Chrome";v="138"`)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")
	
	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求网站首页失败: %w", err)
	}
	defer resp.Body.Close()
	
	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		// 读取响应体以获取服务器返回的具体错误信息
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			// 如果连响应体都读取失败，则返回状态码错误并附上读取错误
			return nil, fmt.Errorf("请求失败，状态码: %d，且读取响应体错误: %v", resp.StatusCode, err)
		}
		// 将更详细的状态信息 (如 "404 Not Found") 和响应体内容一起作为错误返回
		return nil, fmt.Errorf("请求失败，状态: %s, 详情: %s", resp.Status, string(bodyBytes))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	
	// 提取JS文件路径
	jsRegex := regexp.MustCompile(`<script src="(/_next/static/[^"]+\.js)"`)
	matches := jsRegex.FindAllStringSubmatch(string(body), -1)
	
	if len(matches) == 0 {
		return nil, fmt.Errorf("未找到JS文件")
	}
	
	// 收集所有潜在的Action ID
	idSet := make(map[string]struct{})
	idRegex := regexp.MustCompile(`["\']([a-f0-9]{40})["\']{1}`)
	
	for _, match := range matches {
		jsURL := BaseURL + match[1]
		
		// 创建JS文件请求
		jsReq, err := http.NewRequest("GET", jsURL, nil)
		if err != nil {
			continue
		}
		
		// 设置JS文件请求头，保持与首页请求一致
		jsReq.Header.Set("Referer", BaseURL)
		jsReq.Header.Set("Origin", BaseURL)
		jsReq.Header.Set("sec-ch-ua", `"Not)A;Brand";v="8", "Chromium";v="138", "Google Chrome";v="138"`)
		jsReq.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")
		
		// 发送JS文件请求
		jsResp, err := client.Do(jsReq)
		if err != nil {
			continue
		}
		
		jsBody, err := io.ReadAll(jsResp.Body)
		jsResp.Body.Close() // 确保关闭body
		
		if err != nil {
			continue
		}
		
		idMatches := idRegex.FindAllStringSubmatch(string(jsBody), -1)
		for _, idMatch := range idMatches {
			idSet[idMatch[1]] = struct{}{}
		}
	}
	
	// 转换为切片
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	
	if DebugLog {
		fmt.Println("panyq: found", len(ids), "potential Action IDs")
	}
	
	return ids, nil
}

// validateCredentialID 验证credential_action_id
func (p *PanyqPlugin) validateCredentialID(actionID string) bool {
	_, err := p.getCredentials("test", actionID, p.client)
	return err == nil
}

// validateIntermediateID 验证intermediate_action_id
func (p *PanyqPlugin) validateIntermediateID(actionID, testHash, testSha string) bool {
	err := p.performIntermediateStep(actionID, testHash, testSha, "fake_eid_for_validation", p.client)
	return err == nil
}

// validateFinalLinkID 验证final_link_action_id
func (p *PanyqPlugin) validateFinalLinkID(actionID, testEID string) bool {
	responseText, err := p.getRawFinalLinkResponse(actionID, testEID, p.client)
	if err != nil {
		// 记录错误但继续尝试验证，因为Python版本在出现请求异常时返回None，但仍然会尝试验证
		fmt.Println("panyq: 获取响应失败，但仍尝试验证:", err)
		// 即使出错，responseText可能包含部分响应内容
		if responseText == "" {
			return false
		}
	}
	
	// 检查原始响应中是否包含链接相关的关键词
	keywords := []string{"http", "magnet", "aliyundrive", `"url"`}
	for _, kw := range keywords {
		if strings.Contains(responseText, kw) {
			if DebugLog {
				fmt.Println("panyq: found keyword in response:", kw)
			}
			return true
		}
	}
	
	return false
}

// doRequestWithRetry 发送HTTP请求并支持重试
func (p *PanyqPlugin) doRequestWithRetry(client *http.Client, req *http.Request, maxRetries int) (*http.Response, error) {
	var resp *http.Response
	var err error
	
	for i := 0; i <= maxRetries; i++ {
		// 如果不是第一次尝试，等待一段时间
		if i > 0 {
			// 指数退避算法
			backoff := time.Duration(1<<uint(i-1)) * 500 * time.Millisecond
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
			time.Sleep(backoff)
			
			if DebugLog {
				fmt.Printf("panyq: 重试请求 #%d，等待 %v\n", i, backoff)
			}
		}
		
		// 克隆请求，避免重用同一个请求对象
		reqClone := req.Clone(req.Context())
		
		// 发送请求
		resp, err = client.Do(reqClone)
		
		// 如果请求成功或者是不可重试的错误，则退出循环
		if err == nil || !isRetriableError(err) {
			break
		}
	}
	
	return resp, err
}

// isRetriableError 判断错误是否可以重试
func isRetriableError(err error) bool {
	if err == nil {
		return false
	}
	
	// 判断是否是网络错误或超时错误
	if netErr, ok := err.(net.Error); ok {
		return netErr.Timeout() || netErr.Temporary()
	}
	
	// 其他可能需要重试的错误类型
	errStr := err.Error()
	return strings.Contains(errStr, "connection refused") ||
		   strings.Contains(errStr, "connection reset") ||
		   strings.Contains(errStr, "EOF")
}

// getRawFinalLinkResponse 获取最终链接的原始响应文本
func (p *PanyqPlugin) getRawFinalLinkResponse(actionID, eid string, client *http.Client) (string, error) {
	// 检查缓存
	finalLinkCacheLock.RLock()
	cacheKey := fmt.Sprintf("%s:%s", actionID, eid)
	if cachedResponse, ok := finalLinkCache[cacheKey]; ok {
		finalLinkCacheLock.RUnlock()
		if DebugLog {
			fmt.Printf("panyq: 缓存命中 raw final link: %s\n", eid)
		}
		return cachedResponse, nil
	}
	finalLinkCacheLock.RUnlock()

	// 构建URL
	finalURL := fmt.Sprintf("%s/go/%s", BaseURL, eid)
	
	// 构建路由状态树
	routerStateTree := []interface{}{
		"",
		map[string]interface{}{
			"children": []interface{}{
				"go",
				map[string]interface{}{
					"children": []interface{}{
						[]interface{}{"eid", eid, "d"},
						map[string]interface{}{
							"children": []interface{}{"__PAGE__", map[string]interface{}{}, fmt.Sprintf("/go/%s", eid), "refresh"},
						},
					},
				},
			},
		},
		nil,
		nil,
		true,
	}
	
	routerStateTreeJSON, err := json.Marshal(routerStateTree)
	if err != nil {
		return "", err
	}
	
	// 构建请求体
	payload := fmt.Sprintf(`[{"eid":"%s"}]`, eid)
	
	// 创建请求
	req, err := http.NewRequest("POST", finalURL, strings.NewReader(payload))
	if err != nil {
		return "", err
	}
	
	// 设置请求头，保持一致性
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("next-action", actionID)
	req.Header.Set("Referer", finalURL)
	req.Header.Set("Origin", BaseURL)
	req.Header.Set("sec-ch-ua", `"Not)A;Brand";v="8", "Chromium";v="138", "Google Chrome";v="138"`)
	req.Header.Set("next-router-state-tree", url.QueryEscape(string(routerStateTreeJSON)))
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")
	
	// 添加超时设置，与Python版本一致
	if client.Timeout == 0 {
		client = &http.Client{
			Timeout: 15 * time.Second,
			Transport: client.Transport,
		}
	}
	
	// 发送请求并支持重试
	resp, err := p.doRequestWithRetry(client, req, MaxRetries)
	if err != nil {
		// 网络错误等情况下返回空字符串和错误
		if DebugLog {
			fmt.Println("panyq: network error:", err)
		}
		return "", err
	}
	defer resp.Body.Close()
	
	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		if DebugLog {
			fmt.Println("panyq: bad status code:", resp.StatusCode)
		}
		return "", fmt.Errorf("HTTP status code: %d", resp.StatusCode)
	}
	
	// 读取原始响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// 读取错误时返回空字符串和错误
		if DebugLog {
			fmt.Println("panyq: read error:", err)
		}
		return "", err
	}
	
	responseText := string(body)

	// 保存到缓存
	finalLinkCacheLock.Lock()
	finalLinkCache[cacheKey] = responseText
	finalLinkCacheLock.Unlock()
	
	return responseText, nil
}

// getCredentials 获取搜索凭证
func (p *PanyqPlugin) getCredentials(query, actionID string, client *http.Client) (*Credentials, error) {
	// 构建请求体
	payload := fmt.Sprintf(`[{"cat":"all","query":"%s","pageNum":1}]`, query)
	
	// 创建请求
	req, err := http.NewRequest("POST", BaseURL, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	
	// 设置请求头，保持一致性
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("next-action", actionID)
	req.Header.Set("Referer", BaseURL)
	req.Header.Set("Origin", BaseURL)
	req.Header.Set("sec-ch-ua", `"Not)A;Brand";v="8", "Chromium";v="138", "Google Chrome";v="138"`)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")
	
	// 发送请求并支持重试
	resp, err := p.doRequestWithRetry(client, req, MaxRetries)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	
	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	
	// 使用正则表达式提取凭证
	signRegex := regexp.MustCompile(`"sign":"([^"]+)"`)
	shaRegex := regexp.MustCompile(`"sha":"([a-f0-9]{64})"`)
	hashRegex := regexp.MustCompile(`"hash","([^"]+)"`)
	
	signMatch := signRegex.FindStringSubmatch(string(body))
	shaMatch := shaRegex.FindStringSubmatch(string(body))
	hashMatch := hashRegex.FindStringSubmatch(string(body))
	
	if len(signMatch) < 2 || len(shaMatch) < 2 || len(hashMatch) < 2 {
		return nil, fmt.Errorf("提取凭证失败")
	}
	
	return &Credentials{
		Sign: signMatch[1],
		Sha:  shaMatch[1],
		Hash: hashMatch[1],
	}, nil
}

// getSearchResults 获取搜索结果列表
func (p *PanyqPlugin) getSearchResults(sign string, pageNum int, client *http.Client) ([]SearchHit, int, error) {
	// 构建URL
	searchURL := fmt.Sprintf("%s/api/search?sign=%s&page=%d", BaseURL, sign, pageNum)
	
	// 创建请求
	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return nil, 0, err
	}
	
	// 设置请求头，保持一致性
	req.Header.Set("Referer", BaseURL)
	req.Header.Set("Origin", BaseURL)
	req.Header.Set("sec-ch-ua", `"Not)A;Brand";v="8", "Chromium";v="138", "Google Chrome";v="138"`)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")
	
	// 从缓存中获取credential_action_id并添加到请求头
	actionIDCacheLock.RLock()
	if actionID, ok := actionIDCache[ActionIDKeys[0]]; ok {
		req.Header.Set("next-action", actionID)
	}
	actionIDCacheLock.RUnlock()
	
	// 发送请求并支持重试
	resp, err := p.doRequestWithRetry(client, req, MaxRetries)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	
	// 读取响应
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	
	// 解析JSON
	var searchResp SearchResponse
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return nil, 0, err
	}
	
	return searchResp.Data.Hits, searchResp.Data.MaxPageNum, nil
}

// performIntermediateStep 执行中间状态确认
func (p *PanyqPlugin) performIntermediateStep(actionID, hashVal, shaVal, eid string, client *http.Client) error {
	// 构建URL
	intermediateURL := fmt.Sprintf("%s/search/%s", BaseURL, hashVal)
	
	// 构建路由状态树
	routerStateTree := []interface{}{
		"",
		map[string]interface{}{
			"children": []interface{}{
				"search",
				map[string]interface{}{
					"children": []interface{}{
						[]interface{}{"hash", hashVal, "d"},
						map[string]interface{}{
							"children": []interface{}{"__PAGE__", map[string]interface{}{}, fmt.Sprintf("/search/%s", hashVal), "refresh"},
						},
					},
				},
			},
		},
		nil,
		nil,
		true,
	}
	
	routerStateTreeJSON, err := json.Marshal(routerStateTree)
	if err != nil {
		return err
	}
	
	// 构建请求体
	payload := fmt.Sprintf(`[{"eid":"%s","sha":"%s","page_num":"1"}]`, eid, shaVal)
	
	// 创建请求
	req, err := http.NewRequest("POST", intermediateURL, strings.NewReader(payload))
	if err != nil {
		return err
	}
	
	// 设置请求头，保持一致性
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("next-action", actionID)
	req.Header.Set("Referer", intermediateURL)
	req.Header.Set("Origin", BaseURL)
	req.Header.Set("next-router-state-tree", url.QueryEscape(string(routerStateTreeJSON)))
	req.Header.Set("sec-ch-ua", `"Not)A;Brand";v="8", "Chromium";v="138", "Google Chrome";v="138"`)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36")
	
	// 发送请求并支持重试
	resp, err := p.doRequestWithRetry(client, req, MaxRetries)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	// 确认请求成功
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("中间步骤请求失败，状态码: %d", resp.StatusCode)
	}
	
	return nil
}

// getFinalLink 获取最终链接
func (p *PanyqPlugin) getFinalLink(actionID, eid string, client *http.Client) (string, error) {
	// 检查缓存
	finalLinkCacheLock.RLock()
	linkCacheKey := fmt.Sprintf("link:%s:%s", actionID, eid)
	if cachedLink, ok := finalLinkCache[linkCacheKey]; ok {
		finalLinkCacheLock.RUnlock()
		if DebugLog {
			fmt.Printf("panyq: 缓存命中最终链接: %s\n", eid)
		}
		return cachedLink, nil
	}
	finalLinkCacheLock.RUnlock()

	// 获取原始响应
	responseText, err := p.getRawFinalLinkResponse(actionID, eid, client)
	if err != nil {
		return "", err
	}
	
	// 尝试从JSON中提取URL
	lines := strings.Split(responseText, "\n")
	var finalLink string

	if len(lines) > 0 {
		lastLine := lines[len(lines)-1]
		
		var linkData []interface{}
		if err := json.Unmarshal([]byte(lastLine), &linkData); err == nil {
			if len(linkData) > 1 {
				if linkMap, ok := linkData[1].(map[string]interface{}); ok {
					if url, ok := linkMap["url"].(string); ok && url != "" {
						finalLink = url
					}
				}
			}
		}
	}
	
	// 如果JSON解析失败，尝试使用正则表达式
	if finalLink == "" {
		urlRegex := regexp.MustCompile(`(https?://[^\s"'<>]+|magnet:\?[^\s"'<>]+)`)
		urlMatch := urlRegex.FindStringSubmatch(responseText)
		
		if len(urlMatch) > 0 {
			finalLink = urlMatch[0]
		}
	}
	
	if finalLink == "" {
		return "", fmt.Errorf("提取链接失败")
	}

	// 保存链接到缓存
	finalLinkCacheLock.Lock()
	finalLinkCache[linkCacheKey] = finalLink
	finalLinkCacheLock.Unlock()
	
	return finalLink, nil
}

// determineLinkType 根据URL确定链接类型
func (p *PanyqPlugin) determineLinkType(url string) string {
	lowerURL := strings.ToLower(url)
	
	switch {
	case strings.Contains(lowerURL, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(lowerURL, "alipan.com") || strings.Contains(lowerURL, "aliyundrive.com"):
		return "aliyun"
	case strings.Contains(lowerURL, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(lowerURL, "cloud.189.cn"):
		return "tianyi"
	case strings.Contains(lowerURL, "caiyun.139.com") || strings.Contains(lowerURL, "yun.139.com"):
		return "mobile"
	case strings.Contains(lowerURL, "pan.quark.cn"):
		return "quark"
	case strings.Contains(lowerURL, "115.com"):
		return "115"
	case strings.Contains(lowerURL, "weiyun.com"):
		return "weiyun"
	case strings.Contains(lowerURL, "lanzou"):
		return "lanzou"
	case strings.Contains(lowerURL, "jianguoyun.com"):
		return "jianguoyun"
	case strings.Contains(lowerURL, "123pan.com"):
		return "123"
	case strings.Contains(lowerURL, "drive.uc.cn"):
		return "uc"
	case strings.Contains(lowerURL, "mypikpak.com"):
		return "pikpak"
	case strings.HasPrefix(lowerURL, "magnet:"):
		return "magnet"
	case strings.HasPrefix(lowerURL, "ed2k:"):
		return "ed2k"
	default:
		return "others"
	}
}

// extractPassword 从URL或内容中提取密码
func (p *PanyqPlugin) extractPassword(url string, linkType string) string {
	// 百度网盘密码通常在URL后面以?pwd=形式出现
	if linkType == "baidu" {
		if idx := strings.Index(url, "?pwd="); idx >= 0 {
			pwd := url[idx+5:]
			if len(pwd) >= 4 {
				return pwd[:4] // 百度网盘密码通常为4位
			}
			return pwd
		}
	}
	
	// 阿里云盘密码可能在URL参数中
	if linkType == "aliyun" {
		if idx := strings.Index(url, "password="); idx >= 0 {
			pwd := url[idx+9:]
			if endIdx := strings.Index(pwd, "&"); endIdx >= 0 {
				return pwd[:endIdx]
			}
			return pwd
		}
	}
	
	return ""
}

// cleanEscapedHTML 清理HTML转义字符
func (p *PanyqPlugin) cleanEscapedHTML(text string) string {
	// 处理Unicode转义序列
	replacers := map[string]string{
		`\u003Cmark\u003E`:   "",
		`\u003C/mark\u003E`:  "",
		`\u003Cb\u003E`:      "",
		`\u003C/b\u003E`:     "",
		`\u003Cem\u003E`:     "",
		`\u003C/em\u003E`:    "",
		`\u003Cstrong\u003E`: "",
		`\u003C/strong\u003E`: "",
		`\u003Ci\u003E`:      "",
		`\u003C/i\u003E`:     "",
		`\u003Cu\u003E`:      "",
		`\u003C/u\u003E`:     "",
		`\u003Cbr\u003E`:     " ",
		`\u003Cbr/\u003E`:    " ",
		`\u003Cbr /\u003E`:   " ",
	}
	
	result := text
	for old, new := range replacers {
		result = strings.ReplaceAll(result, old, new)
	}
	
	// 处理实际的HTML标签
	htmlReplacers := map[string]string{
		`<mark>`:   "",
		`</mark>`:  "",
		`<b>`:      "",
		`</b>`:     "",
		`<em>`:     "",
		`</em>`:    "",
		`<strong>`: "",
		`</strong>`: "",
		`<i>`:      "",
		`</i>`:     "",
		`<u>`:      "",
		`</u>`:     "",
		`<br>`:     " ",
		`<br/>`:    " ",
		`<br />`:   " ",
	}
	
	for old, new := range htmlReplacers {
		result = strings.ReplaceAll(result, old, new)
	}
	
	// 处理已解码的Unicode转义序列
	decodedReplacers := map[string]string{
		string([]byte{0x3C, 0x6D, 0x61, 0x72, 0x6B, 0x3E}):                 "", // <mark>
		string([]byte{0x3C, 0x2F, 0x6D, 0x61, 0x72, 0x6B, 0x3E}):           "", // </mark>
		string([]byte{0x3C, 0x62, 0x3E}):                                   "", // <b>
		string([]byte{0x3C, 0x2F, 0x62, 0x3E}):                             "", // </b>
		string([]byte{0x3C, 0x65, 0x6D, 0x3E}):                             "", // <em>
		string([]byte{0x3C, 0x2F, 0x65, 0x6D, 0x3E}):                       "", // </em>
		string([]byte{0x3C, 0x73, 0x74, 0x72, 0x6F, 0x6E, 0x67, 0x3E}):     "", // <strong>
		string([]byte{0x3C, 0x2F, 0x73, 0x74, 0x72, 0x6F, 0x6E, 0x67, 0x3E}): "", // </strong>
	}
	
	for old, new := range decodedReplacers {
		result = strings.ReplaceAll(result, old, new)
	}
	
	return result
}

// extractTitle 从描述中提取标题
func (p *PanyqPlugin) extractTitle(desc string) string {
	// 先清理HTML标签
	cleanDesc := p.cleanEscapedHTML(desc)
	
	// 尝试匹配标题
	// 1. 尝试匹配《》内的内容
	titleRegex := regexp.MustCompile(`《([^》]+)》`)
	if matches := titleRegex.FindStringSubmatch(cleanDesc); len(matches) > 1 {
		return matches[1]
	}
	
	// 2. 尝试匹配【】内的内容
	titleRegex = regexp.MustCompile(`【([^】]+)】`)
	if matches := titleRegex.FindStringSubmatch(cleanDesc); len(matches) > 1 {
		return matches[1]
	}
	
	// 3. 尝试提取开头的一段（到第一个分隔符为止）
	parts := strings.Split(cleanDesc, "✔")
	if len(parts) > 0 && len(parts[0]) > 0 {
		return strings.TrimSpace(parts[0])
	}
	
	// 如果以上方法都无法提取标题，则取前30个字符作为标题
	if len(cleanDesc) > 30 {
		return strings.TrimSpace(cleanDesc[:30]) + "..."
	}
	
	return strings.TrimSpace(cleanDesc)
}

// 在init函数中注册插件
func init() {
	plugin.RegisterGlobalPlugin(NewPanyqPlugin())
	
	// 启动缓存清理
	go startCacheCleaner()
}

// 启动缓存清理器
func startCacheCleaner() {
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		if DebugLog {
			fmt.Println("panyq: 开始清理缓存")
		}
		
		// 清理finalLinkCache
		finalLinkCacheLock.Lock()
		finalLinkCache = make(map[string]string)
		finalLinkCacheLock.Unlock()
		
		// 清理searchResultCache
		searchResultCacheLock.Lock()
		searchResultCache = make(map[string][]model.SearchResult)
		searchResultCacheLock.Unlock()
		
		if DebugLog {
			fmt.Println("panyq: 缓存清理完成")
		}
	}
}

// loadActionIDsFromFile 从文件加载Action IDs
func (p *PanyqPlugin) loadActionIDsFromFile() (map[string]string, error) {
	configPath := filepath.Join(".", ConfigFileName)
	
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}
	
	var ids map[string]string
	if err := json.Unmarshal(data, &ids); err != nil {
		return nil, err
	}
	
	// 验证所有必需的键是否存在
	for _, key := range ActionIDKeys {
		if _, ok := ids[key]; !ok {
			return nil, fmt.Errorf("缓存文件中缺少键: %s", key)
		}
	}
	
	return ids, nil
}

// saveActionIDsToFile 保存Action IDs到文件
func (p *PanyqPlugin) saveActionIDsToFile(ids map[string]string) error {
	data, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	
	configPath := filepath.Join(".", ConfigFileName)
	return os.WriteFile(configPath, data, 0644)
}
