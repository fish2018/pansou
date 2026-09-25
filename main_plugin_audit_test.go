package main

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"pansou/config"
	"pansou/plugin"
	"pansou/util"
)

// 插件源健康审计：逐个调用已注册插件，把结果分为
//
//	健康      —— 取到了结果
//	抓取失败  —— 返回了错误（错误文本可用于判断是站点改版还是本机网络）
//	空结果    —— 无错误且无结果：需第二轮复核（第一轮可能只是响应超时返回了部分空结果）
//
// 判定原则：只有"解析/结构类"错误才能断定插件本身失效；网络类错误在本机不可判定，
// 必须标注为待确认，不能当成插件坏了。
//
// 默认跳过，需要真实网络与代理：
//
//	AUDIT_KEYWORD=仙逆 PROXY_WIRING_LIVE_TEST=1 go test -run TestPluginSourceAudit -v .
func TestPluginSourceAudit(t *testing.T) {
	if os.Getenv("AUDIT_PLUGIN_SOURCES") != "1" {
		t.Skip("set AUDIT_PLUGIN_SOURCES=1 to audit every registered plugin against the live sources")
	}

	config.Init()
	util.InitHTTPClient()

	keyword := os.Getenv("AUDIT_KEYWORD")
	if keyword == "" {
		keyword = "仙逆"
	}

	plugins := plugin.GetRegisteredPlugins()
	t.Logf("共 %d 个已注册插件，关键词 %q", len(plugins), keyword)

	type outcome struct {
		name     string
		count    int
		err      error
		duration time.Duration
	}

	run := func(ext map[string]interface{}) map[string]outcome {
		results := sync.Map{}
		var wg sync.WaitGroup
		for _, p := range plugins {
			wg.Add(1)
			go func(p plugin.AsyncSearchPlugin) {
				defer wg.Done()
				start := time.Now()
				res, err := p.Search(keyword, ext)
				results.Store(p.Name(), outcome{p.Name(), len(res), err, time.Since(start)})
			}(p)
		}
		wg.Wait()

		collected := map[string]outcome{}
		results.Range(func(k, v interface{}) bool {
			collected[k.(string)] = v.(outcome)
			return true
		})
		return collected
	}

	// 第一轮：拿错误最有效（错误会立刻返回，不受响应超时影响）
	first := run(map[string]interface{}{"refresh": true})

	// 等待后台处理完成，再复核"无错误但空结果"的插件：它们可能只是第一轮
	// 响应超时返回了部分空结果，并不代表源失效
	t.Logf("等待 35s 让后台处理完成，随后复核空结果插件")
	time.Sleep(35 * time.Second)
	second := run(nil)

	healthy, failed, empty := []string{}, []string{}, []string{}
	byName := make([]outcome, 0, len(plugins))
	for _, o := range first {
		byName = append(byName, o)
	}
	sort.Slice(byName, func(i, j int) bool { return byName[i].name < byName[j].name })

	for _, o := range byName {
		confirm := second[o.name]
		switch {
		case o.err != nil:
			failed = append(failed, o.name)
			fmt.Printf("FAILED\t%s\t%v\t%s\n", o.name, o.duration.Round(time.Millisecond), o.err)
		case o.count > 0 || confirm.count > 0:
			healthy = append(healthy, o.name)
			fmt.Printf("OK\t%s\t%d条\t%s\n", o.name, max(o.count, confirm.count), o.duration.Round(time.Millisecond))
		default:
			empty = append(empty, o.name)
			fmt.Printf("EMPTY\t%s\t%s\n", o.name, o.duration.Round(time.Millisecond))
		}
	}

	fmt.Printf("\n汇总: 健康 %d，抓取失败 %d，复核后仍空 %d\n", len(healthy), len(failed), len(empty))
	if len(failed) > 0 {
		fmt.Printf("失败插件: %v\n", failed)
	}
	if len(empty) > 0 {
		fmt.Printf("空结果插件: %v\n", empty)
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
