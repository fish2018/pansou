package qqpd

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util"
	"pansou/util/json"

	"github.com/gin-gonic/gin"
)

// 插件配置参数（代码内配置）
const (
	MaxConcurrentUsers    = 10    // 最多使用的用户数
	MaxConcurrentChannels = 50    // 最大并发频道数
	DebugLog              = false // 调试日志开关（临时开启排查问题）
)

// 存储目录 - 从环境变量动态获取
var StorageDir string

// 初始化存储目录

// HTML模板（完整的管理页面）
const HTMLTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>PanSou QQ频道搜索配置</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body { 
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            padding: 20px;
        }
        .container {
            max-width: 800px;
            margin: 0 auto;
            background: white;
            border-radius: 16px;
            box-shadow: 0 20px 60px rgba(0,0,0,0.3);
            overflow: hidden;
        }
        .header {
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            padding: 30px;
            text-align: center;
        }
        .section {
            padding: 30px;
            border-bottom: 1px solid #eee;
        }
        .section:last-child { border-bottom: none; }
        .section-title {
            font-size: 18px;
            font-weight: bold;
            margin-bottom: 15px;
            color: #333;
        }
        .status-box {
            background: #f8f9fa;
            padding: 20px;
            border-radius: 8px;
            margin-bottom: 15px;
        }
        .status-item {
            display: flex;
            justify-content: space-between;
            padding: 8px 0;
        }
        .qrcode-container {
            text-align: center;
            padding: 20px;
        }
        .qrcode-img {
            max-width: 200px;
            border: 2px solid #ddd;
            border-radius: 8px;
        }
        .btn {
            padding: 10px 20px;
            border: none;
            border-radius: 6px;
            cursor: pointer;
            font-size: 14px;
            transition: all 0.3s;
        }
        .btn-primary {
            background: #667eea;
            color: white;
        }
        .btn-primary:hover { background: #5568d3; }
        .btn-danger {
            background: #f56565;
            color: white;
        }
        .btn-danger:hover { background: #e53e3e; }
        .btn-secondary {
            background: #e2e8f0;
            color: #333;
        }
        .btn-secondary:hover { background: #cbd5e0; }
        textarea {
            width: 100%;
            padding: 10px 15px;
            border: 1px solid #ddd;
            border-radius: 6px;
            font-size: 14px;
            resize: vertical;
            font-family: monospace;
        }
        .test-results {
            max-height: 300px;
            overflow-y: auto;
            background: #f8f9fa;
            padding: 15px;
            border-radius: 6px;
            margin-top: 10px;
        }
        .hidden { display: none; }
        .alert {
            padding: 12px 15px;
            border-radius: 6px;
            margin: 10px 0;
        }
        .alert-success {
            background: #c6f6d5;
            color: #22543d;
        }
        .alert-error {
            background: #fed7d7;
            color: #742a2a;
        }
        .api-code {
            background: #2d3748;
            color: #68d391;
            padding: 10px;
            border-radius: 6px;
            font-family: 'Courier New', monospace;
            font-size: 12px;
            overflow-x: auto;
            margin: 10px 0;
            white-space: pre-wrap;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>🔍 PanSou QQ频道搜索</h1>
            <p>配置你的专属搜索服务</p>
            <p style="font-size: 12px; margin-top: 10px; opacity: 0.8;">
                🔗 当前地址: <span id="current-url">HASH_PLACEHOLDER</span>
            </p>
        </div>

        <div class="section" id="login-section">
            <div class="section-title">📱 登录状态</div>
            
            <div id="logged-in-view" class="hidden">
                <div style="text-align: center; padding: 20px;">
                    <div style="width: 100px; height: 100px; margin: 0 auto 15px; border-radius: 50%; background: linear-gradient(135deg, #667eea 0%, #764ba2 100%); display: flex; align-items: center; justify-content: center; color: white; font-size: 36px; font-weight: bold;">
                        <span id="qq-avatar">QQ</span>
                    </div>
                </div>
                <div class="status-box">
                    <div class="status-item">
                        <span>状态</span>
                        <span><strong style="color: #48bb78;">✅ 已登录</strong></span>
                    </div>
                    <div class="status-item">
                        <span>QQ号</span>
                        <span id="qq-masked">-</span>
                    </div>
                    <div class="status-item">
                        <span>登录时间</span>
                        <span id="login-time">-</span>
                    </div>
                    <div class="status-item">
                        <span>有效期</span>
                        <span id="expire-info">-</span>
                    </div>
                </div>
                <button class="btn btn-danger" onclick="logout()">退出登录</button>
            </div>

            <div id="not-logged-in-view" class="hidden">
                <div class="qrcode-container">
                    <img id="qrcode-img" class="qrcode-img" src="" alt="二维码">
                    <p style="margin-top: 10px; color: #666;">
                        请使用手机QQ扫描二维码登录
                    </p>
                    <p style="font-size: 12px; color: #999;">扫码后自动检测登录状态</p>
                    <button class="btn btn-secondary" onclick="refreshQRCode()" style="margin-top: 10px;">
                        刷新二维码
                    </button>
                </div>
            </div>
        </div>

        <div class="section" id="channels-section">
            <div class="section-title">📋 频道管理 (<span id="channel-count">0</span> 个)</div>
            
            <div id="alert-box"></div>
            
            <p style="margin-bottom: 10px; color: #666;">每行一个频道号或链接，保存时自动去重</p>
            <textarea id="channels-textarea" rows="10" placeholder="pd97631607
kuake12345
languan8K115"></textarea>
            
            <button class="btn btn-primary" onclick="saveChannels()" style="margin-top: 10px;">保存频道配置</button>
        </div>

        <div class="section" id="test-section">
            <div class="section-title">🔍 测试搜索(限制返回10条数据)</div>
            
            <div style="display: flex; gap: 10px;">
                <input type="text" id="search-keyword" placeholder="输入关键词测试搜索" style="flex: 1; padding: 10px; border: 1px solid #ddd; border-radius: 6px;">
                <button class="btn btn-primary" onclick="testSearch()">搜索</button>
            </div>

            <div id="search-results" class="test-results hidden"></div>
        </div>

        <div class="section">
            <div class="section-title">📖 API调用说明</div>
            
            <p style="margin-bottom: 15px;">你可以通过API程序化管理频道和搜索：</p>

            <details>
                <summary style="cursor: pointer; padding: 10px 0; font-weight: bold;">获取状态</summary>
                <div class="api-code">curl -X POST https://your-domain.com/qqpd/HASH_PLACEHOLDER \
  -H "Content-Type: application/json" \
  -d '{"action": "get_status"}'</div>
            </details>

            <details>
                <summary style="cursor: pointer; padding: 10px 0; font-weight: bold;">设置频道列表</summary>
                <div class="api-code">curl -X POST https://your-domain.com/qqpd/HASH_PLACEHOLDER \
  -H "Content-Type: application/json" \
  -d '{"action": "set_channels", "channels": ["pd97631607", "kuake12345"]}'</div>
            </details>

            <details>
                <summary style="cursor: pointer; padding: 10px 0; font-weight: bold;">测试搜索</summary>
                <div class="api-code">curl -X POST https://your-domain.com/qqpd/HASH_PLACEHOLDER \
  -H "Content-Type: application/json" \
  -d '{"action": "test_search", "keyword": "遮天"}'</div>
            </details>
        </div>
    </div>

    <script>
        const HASH = 'HASH_PLACEHOLDER';
        const API_URL = '/qqpd/' + HASH;
        let statusCheckInterval = null;
        let loginCheckInterval = null;

        window.onload = function() {
            updateStatus();
            startStatusPolling();
        };

        function startStatusPolling() {
            statusCheckInterval = setInterval(updateStatus, 3000);
        }

        function startLoginPolling() {
            if (loginCheckInterval) return; // 避免重复启动
            loginCheckInterval = setInterval(checkLogin, 2000);
        }

        function stopLoginPolling() {
            if (loginCheckInterval) {
                clearInterval(loginCheckInterval);
                loginCheckInterval = null;
            }
        }

        async function postAction(action, extraData = {}) {
            try {
                const response = await fetch(API_URL, {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify({ action: action, ...extraData })
                });
                return await response.json();
            } catch (error) {
                console.error('请求失败:', error);
                return { success: false, message: '请求失败: ' + error.message };
            }
        }

        async function updateStatus() {
            const result = await postAction('get_status');
            if (result.success && result.data) {
                const data = result.data;
                
                if (data.logged_in === true && data.status === 'active') {
                    // 已登录：显示用户信息，隐藏二维码
                    document.getElementById('logged-in-view').classList.remove('hidden');
                    document.getElementById('not-logged-in-view').classList.add('hidden');
                    
                    // 更新用户信息
                    const qqMasked = data.qq_masked || 'QQ';
                    document.getElementById('qq-masked').textContent = qqMasked;
                    document.getElementById('login-time').textContent = data.login_time || '-';
                    document.getElementById('expire-info').textContent = '剩余 ' + (data.expires_in_days || 0) + ' 天';
                    
                    // 显示QQ号首位作为头像
                    const firstChar = qqMasked.charAt(0) || 'Q';
                    document.getElementById('qq-avatar').textContent = firstChar;
                    
                    // 停止登录检测
                    stopLoginPolling();
                } else {
                    // 未登录：显示二维码，隐藏用户信息
                    document.getElementById('logged-in-view').classList.add('hidden');
                    document.getElementById('not-logged-in-view').classList.remove('hidden');
                    
                    if (data.qrcode_base64) {
                        document.getElementById('qrcode-img').src = data.qrcode_base64;
                    }
                    
                    // 启动登录检测（每2秒检查一次）
                    startLoginPolling();
                }

                updateChannelList(data.channels || []);
            }
        }

        async function checkLogin() {
            const result = await postAction('check_login');
            if (result.success && result.data) {
                if (result.data.login_status === 'success') {
                    // 登录成功，停止轮询并刷新状态
                    stopLoginPolling();
                    showAlert('登录成功！');
                    updateStatus();
                }
            }
        }

        function updateChannelList(channels) {
            const textarea = document.getElementById('channels-textarea');
            const count = document.getElementById('channel-count');
            
            count.textContent = channels.length;
            
            // 只在用户没有聚焦输入框时更新内容
            if (document.activeElement !== textarea) {
                textarea.value = channels.join('\n');
            }
        }

        function escapeHTML(value) {
            const element = document.createElement('span');
            element.textContent = String(value || '');
            return element.innerHTML;
        }

        function showAlert(message, type = 'success') {
            const alertBox = document.getElementById('alert-box');
            alertBox.innerHTML = '<div class="alert alert-' + type + '">' + escapeHTML(message) + '</div>';
            setTimeout(() => {
                alertBox.innerHTML = '';
            }, 3000);
        }

        async function refreshQRCode() {
            const result = await postAction('refresh_qrcode');
            if (result.success) {
                showAlert(result.message);
                updateStatus();
                // 启动登录检测
                startLoginPolling();
            } else {
                showAlert(result.message, 'error');
            }
        }

        async function logout() {
            if (!confirm('确定要退出登录吗？')) return;
            
            const result = await postAction('logout');
            if (result.success) {
                showAlert(result.message);
                updateStatus();
            } else {
                showAlert(result.message, 'error');
            }
        }

        async function saveChannels() {
            const textarea = document.getElementById('channels-textarea');
            const channelsText = textarea.value.trim();
            
            const channels = channelsText
                .split('\n')
                .map(line => line.trim())
                .filter(line => line.length > 0);
            
            const result = await postAction('set_channels', { channels });
            if (result.success) {
                const warning = result.data.warning;
                showAlert(result.message + (warning ? '\n' + warning : ''), warning ? 'error' : 'success');
                updateStatus();
            } else {
                showAlert(result.message, 'error');
            }
        }

        async function testSearch() {
            const keyword = document.getElementById('search-keyword').value.trim();
            
            if (!keyword) {
                showAlert('请输入搜索关键词', 'error');
                return;
            }

            const resultsDiv = document.getElementById('search-results');
            resultsDiv.classList.remove('hidden');
            resultsDiv.innerHTML = '<div>🔍 搜索中...</div>';

            const result = await postAction('test_search', { keyword });
            
            if (result.success) {
                const results = result.data.results || [];
                
                if (results.length === 0) {
                    resultsDiv.innerHTML = '<p style="text-align: center; color: #999;">未找到结果</p>';
                    return;
                }

                let html = '<p><strong>找到 ' + result.data.total_results + ' 条结果</strong></p>';
                if (result.data.warning) {
                    html += '<p style="color: #c53030; white-space: pre-wrap;">' + escapeHTML(result.data.warning) + '</p>';
                }
                results.forEach((item, index) => {
                    html += '<div style="margin: 15px 0; padding: 10px; background: white; border-radius: 6px;">';
                    html += '<p><strong>' + (index + 1) + '. ' + item.title + '</strong></p>';
                    item.links.forEach(link => {
                        html += '<p style="font-size: 12px; color: #666; margin: 5px 0; word-break: break-all;">';
                        html += '[' + link.type + '] ' + link.url;
                        if (link.password) html += ' 密码: ' + link.password;
                        html += '</p>';
                    });
                    html += '</div>';
                });
                resultsDiv.innerHTML = html;
            } else {
                resultsDiv.innerHTML = '<p style="color: red; white-space: pre-wrap;">' + escapeHTML(result.message) + '</p>';
            }
        }

        document.getElementById('search-keyword').addEventListener('keypress', function(e) {
            if (e.key === 'Enter') testSearch();
        });
    </script>
</body>
</html>`

// QQPDPlugin 插件结构
type QQPDPlugin struct {
	*plugin.BaseAsyncPlugin
	users       sync.Map // 内存缓存：hash -> *User
	mu          sync.RWMutex
	initialized bool // 初始化状态标记
	httpClient  *http.Client
}

// User 用户数据结构
type User struct {
	Hash            string            `json:"hash"`
	QQMasked        string            `json:"qq_masked"`
	Cookie          string            `json:"cookie"`
	Status          string            `json:"status"`
	Channels        []string          `json:"channels"`
	ChannelGuildIDs map[string]string `json:"channel_guild_ids"` // 频道号->guild_id映射（持久化缓存）
	CreatedAt       time.Time         `json:"created_at"`
	LoginAt         time.Time         `json:"login_at"`
	ExpireAt        time.Time         `json:"expire_at"`
	LastAccessAt    time.Time         `json:"last_access_at"`

	// 二维码相关（不持久化）
	QRCodeCache     []byte    `json:"-"` // 二维码缓存
	QRCodeCacheTime time.Time `json:"-"` // 二维码生成时间
	Qrsig           string    `json:"-"` // qrsig（用于登录检测）
}

// ChannelTask 频道搜索任务
type ChannelTask struct {
	ChannelID string // 频道号
	GuildID   string // 真实的guild_id（从缓存或实时获取）
	UserHash  string // 分配给哪个用户
	Cookie    string // 使用的Cookie
}

func init() {
	p := &QQPDPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("qqpd", 3),
	}

	plugin.RegisterGlobalPlugin(p)
}

// Initialize 实现 InitializablePlugin 接口，延迟初始化插件
func (p *QQPDPlugin) Initialize() error {
	if p.initialized {
		return nil
	}

	// 初始化存储目录路径
	cachePath := os.Getenv("CACHE_PATH")
	if cachePath == "" {
		cachePath = "./cache"
	}
	StorageDir = filepath.Join(cachePath, "qqpd_users")

	// 初始化存储目录
	if err := os.MkdirAll(StorageDir, 0755); err != nil {
		return fmt.Errorf("创建存储目录失败: %v", err)
	}

	// 加载所有用户到内存
	p.loadAllUsers()

	// 启动定期清理任务
	go p.startCleanupTask()

	p.initialized = true
	return nil
}

// ============ 插件接口实现 ============

// SkipServiceFilter 返回是否跳过Service层的关键词过滤
// 注释掉：让Service层来处理过滤，Service层会根据每个链接的标题进行精确过滤
// func (p *QQPDPlugin) SkipServiceFilter() bool {
// 	return true
// }

// RegisterWebRoutes 注册Web路由
func (p *QQPDPlugin) RegisterWebRoutes(router *gin.RouterGroup) {
	qqpd := router.Group("/qqpd")
	qqpd.GET("/:param", p.handleManagePage)
	qqpd.POST("/:param", p.handleManagePagePOST)

	fmt.Printf("[QQPD] Web路由已注册: /qqpd/:param\n")
}

// Search 执行搜索并返回结果（兼容性方法）
func (p *QQPDPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
func (p *QQPDPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	if DebugLog {
		fmt.Printf("[QQPD] ========== 开始搜索: %s ==========\n", keyword)
	}

	// 1. 获取所有有效用户
	users := p.getActiveUsers()
	if DebugLog {
		fmt.Printf("[QQPD] 找到 %d 个有效用户\n", len(users))
	}

	if len(users) == 0 {
		if DebugLog {
			fmt.Printf("[QQPD] 没有有效用户，返回空结果\n")
		}
		return model.PluginSearchResult{Results: []model.SearchResult{}, IsFinal: true}, nil
	}

	// 2. 限制用户数量（取最近活跃的）
	if len(users) > MaxConcurrentUsers {
		sort.Slice(users, func(i, j int) bool {
			return users[i].LastAccessAt.After(users[j].LastAccessAt)
		})
		users = users[:MaxConcurrentUsers]
		if DebugLog {
			fmt.Printf("[QQPD] 限制用户数量为: %d\n", MaxConcurrentUsers)
		}
	}

	// 3. 收集并去重频道，智能分配给用户
	tasks := p.buildChannelTasks(users)
	if DebugLog {
		fmt.Printf("[QQPD] 生成 %d 个频道任务（去重后）\n", len(tasks))
		for i, task := range tasks {
			if i < 5 { // 只打印前5个
				fmt.Printf("[QQPD]   任务%d: 频道=%s, 用户=%s\n", i+1, task.ChannelID, task.UserHash[:8]+"...")
			}
		}
	}

	// 4. 并发执行所有任务
	results, searchErr := p.executeTasks(tasks, keyword)
	message := ""
	if searchErr != nil {
		message = searchErr.Error()
		if len(results) == 0 {
			return model.PluginSearchResult{Results: []model.SearchResult{}, IsFinal: true, Message: message}, searchErr
		}
		fmt.Printf("[QQPD] 部分频道搜索失败: %v\n", searchErr)
	}
	if DebugLog {
		fmt.Printf("[QQPD] 所有任务完成，获得 %d 条原始结果\n", len(results))
	}

	// 5. 不在插件内过滤，交给Service层处理（Service层会根据每个链接的标题精确过滤）
	// filtered := plugin.FilterResultsByKeyword(results, keyword)
	if DebugLog {
		fmt.Printf("[QQPD] 返回 %d 条结果（交由Service层过滤）\n", len(results))
		fmt.Printf("[QQPD] ========== 搜索完成 ==========\n")
	}

	return model.PluginSearchResult{
		Results: results, // 返回原始结果，不过滤
		IsFinal: true,
		Message: message,
	}, nil
}

// ============ 内存缓存管理 ============

// loadAllUsers 启动时加载所有用户到内存
func (p *QQPDPlugin) loadAllUsers() {
	files, err := ioutil.ReadDir(StorageDir)
	if err != nil {
		return
	}

	count := 0
	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" {
			continue
		}

		filePath := filepath.Join(StorageDir, file.Name())
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			continue
		}

		var user User
		if err := json.Unmarshal(data, &user); err != nil {
			continue
		}
		for channelNumber, guildID := range user.ChannelGuildIDs {
			if !validGuildID(guildID) {
				delete(user.ChannelGuildIDs, channelNumber)
			}
		}

		// 加载到内存
		p.users.Store(user.Hash, &user)
		count++
	}

	fmt.Printf("[QQPD] 已加载 %d 个用户到内存\n", count)
}

// getUserByHash 获取用户（从内存）
func (p *QQPDPlugin) getUserByHash(hash string) (*User, bool) {
	value, ok := p.users.Load(hash)
	if !ok {
		return nil, false
	}
	return value.(*User), true
}

// saveUser 保存用户（内存+文件）
func (p *QQPDPlugin) saveUser(user *User) error {
	// 更新内存
	p.users.Store(user.Hash, user)

	// 持久化到文件
	return p.persistUser(user)
}

// persistUser 持久化用户到文件
func (p *QQPDPlugin) persistUser(user *User) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.persistUserLocked(user)
}

func (instance *QQPDPlugin) persistUserLocked(user *User) error {
	filePath := filepath.Join(StorageDir, user.Hash+".json")

	data, err := json.MarshalIndent(user, "", "  ")
	if err != nil {
		return err
	}

	return ioutil.WriteFile(filePath, data, 0644)
}

// deleteUser 删除用户（内存+文件）
func (p *QQPDPlugin) deleteUser(hash string) error {
	// 从内存删除
	p.users.Delete(hash)

	// 从文件删除
	filePath := filepath.Join(StorageDir, hash+".json")
	return os.Remove(filePath)
}

// getActiveUsers 获取有效的活跃用户
func (p *QQPDPlugin) getActiveUsers() []*User {
	var users []*User

	totalUsers := 0
	activeUsers := 0
	expiredUsers := 0
	noChannelUsers := 0

	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)
		totalUsers++

		// 双重过滤
		if user.Status != "active" {
			if DebugLog && totalUsers <= 3 {
				fmt.Printf("[QQPD]   用户%s: 状态=%s (非active，跳过)\n", user.Hash[:8]+"...", user.Status)
			}
			return true
		}

		// 检查Cookie是否过期（根据ExpireAt时间判断）
		if !user.ExpireAt.IsZero() && time.Now().After(user.ExpireAt) {
			// Cookie已过期，标记用户状态为过期
			expiredUsers++
			user.Status = "expired"
			user.Cookie = "" // 清空Cookie
			p.saveUser(user)
			if DebugLog && expiredUsers <= 3 {
				fmt.Printf("[QQPD]   用户%s: Cookie已过期 (过期时间: %s)\n", user.Hash[:8]+"...", user.ExpireAt.Format("2006-01-02 15:04:05"))
			}
			return true
		}

		if len(user.Channels) == 0 {
			noChannelUsers++
			if DebugLog && noChannelUsers <= 3 {
				fmt.Printf("[QQPD]   用户%s: 频道数=0 (跳过)\n", user.Hash[:8]+"...")
			}
			return true
		}

		// 通过所有过滤
		activeUsers++
		if DebugLog && activeUsers <= 3 {
			remainingDays := 0
			if !user.ExpireAt.IsZero() {
				remainingDays = int(time.Until(user.ExpireAt).Hours() / 24)
			}
			fmt.Printf("[QQPD]   用户%s: 有效 (频道数=%d, 剩余有效期=%d天)\n", user.Hash[:8]+"...", len(user.Channels), remainingDays)
		}
		users = append(users, user)
		return true
	})

	if DebugLog {
		fmt.Printf("[QQPD] 用户统计: 总数=%d, 有效=%d, 已过期=%d, 无频道=%d\n",
			totalUsers, activeUsers, expiredUsers, noChannelUsers)
	}

	return users
}

// ============ HTTP路由处理 ============

// handleManagePage GET路由处理（合并QQ号转hash和显示页面）
func (p *QQPDPlugin) handleManagePage(c *gin.Context) {
	param := c.Param("param")

	// 判断是QQ号还是hash（hash是64字符的十六进制）
	if len(param) == 64 && p.isHexString(param) {
		// 这是hash，直接显示管理页面
		html := strings.ReplaceAll(HTMLTemplate, "HASH_PLACEHOLDER", param)
		c.Data(200, "text/html; charset=utf-8", []byte(html))
	} else {
		// 这是QQ号，计算hash并重定向
		hash := p.generateHash(param)
		c.Redirect(302, "/qqpd/"+hash)
	}
}

// handleManagePagePOST POST路由处理
func (p *QQPDPlugin) handleManagePagePOST(c *gin.Context) {
	hash := c.Param("param")

	// 读取完整的请求体到map
	var reqData map[string]interface{}
	if err := c.ShouldBindJSON(&reqData); err != nil {
		respondError(c, "无效的请求格式: "+err.Error())
		return
	}

	// 获取action字段
	action, ok := reqData["action"].(string)
	if !ok || action == "" {
		respondError(c, "缺少action字段")
		return
	}

	// 根据action路由到不同的处理函数
	switch action {
	case "get_status":
		p.handleGetStatus(c, hash)
	case "refresh_qrcode":
		p.handleRefreshQRCode(c, hash)
	case "logout":
		p.handleLogout(c, hash)
	case "set_channels":
		p.handleSetChannelsWithData(c, hash, reqData)
	case "test_search":
		p.handleTestSearchWithData(c, hash, reqData)
	case "manual_login":
		// 测试用：手动设置登录状态
		p.handleManualLogin(c, hash, reqData)
	case "check_login":
		// 检查登录状态（扫码后调用）
		p.handleCheckLogin(c, hash)
	default:
		respondError(c, "未知的操作类型: "+action)
	}
}

// ============ POST Action处理 ============

// handleGetStatus 获取状态
func (p *QQPDPlugin) handleGetStatus(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)

	if !exists {
		// 创建新用户（内存+文件）
		user = &User{
			Hash:         hash,
			Status:       "pending",
			Channels:     []string{},
			CreatedAt:    time.Now(),
			LastAccessAt: time.Now(),
		}
		p.saveUser(user)
	} else {
		// 更新最后访问时间
		user.LastAccessAt = time.Now()
		p.saveUser(user)
	}

	// 检查登录状态（简化逻辑）
	loggedIn := false
	if user.Status == "active" && user.Cookie != "" {
		// 状态是active且有Cookie，刷新cookies（更新uuid等动态字段）
		refreshedCookie := p.refreshCookie(user.Cookie)
		if refreshedCookie != user.Cookie {
			user.Cookie = refreshedCookie
			p.saveUser(user)
		}
		loggedIn = true
	} else if user.Status == "active" && user.Cookie == "" {
		// 状态是active但Cookie为空，异常情况，重置为pending
		if DebugLog {
			fmt.Printf("[QQPD] 用户 %s 状态异常（active但Cookie为空），重置为pending\n", hash[:8]+"...")
		}
		user.Status = "pending"
		user.QQMasked = ""
		p.saveUser(user)
	}

	// 生成二维码（如果需要）
	var qrcodeBase64 string
	if !loggedIn {
		// 使用缓存的二维码（30秒内有效）
		if user.QRCodeCache != nil && time.Since(user.QRCodeCacheTime) < 30*time.Second {
			qrcodeBase64 = "data:image/png;base64," + base64.StdEncoding.EncodeToString(user.QRCodeCache)
			if DebugLog {
				fmt.Printf("[QQPD] 使用缓存的二维码（还剩 %.0f 秒）\n", 30-time.Since(user.QRCodeCacheTime).Seconds())
			}
		} else {
			// 生成新二维码
			qrcodeBytes, qrsig, err := p.generateQRCodeWithSig()
			if err != nil {
				fmt.Printf("[QQPD] 生成二维码失败: %v\n", err)
				qrcodeBase64 = ""
			} else {
				qrcodeBase64 = "data:image/png;base64," + base64.StdEncoding.EncodeToString(qrcodeBytes)
				// 缓存二维码和qrsig
				user.QRCodeCache = qrcodeBytes
				user.QRCodeCacheTime = time.Now()
				user.Qrsig = qrsig
				if DebugLog {
					fmt.Printf("[QQPD] 生成新二维码并缓存30秒\n")
				}
			}
		}
	}

	// 计算剩余天数
	expiresInDays := 0
	if !user.ExpireAt.IsZero() {
		expiresInDays = int(time.Until(user.ExpireAt).Hours() / 24)
		if expiresInDays < 0 {
			expiresInDays = 0
		}
	}

	respondSuccess(c, "获取成功", gin.H{
		"hash":            hash,
		"logged_in":       loggedIn,
		"status":          user.Status,
		"qq_masked":       user.QQMasked,
		"login_time":      user.LoginAt.Format("2006-01-02 15:04:05"),
		"expire_time":     user.ExpireAt.Format("2006-01-02 15:04:05"),
		"expires_in_days": expiresInDays,
		"channels":        user.Channels,
		"channel_count":   len(user.Channels),
		"qrcode_base64":   qrcodeBase64,
	})
}

// handleRefreshQRCode 刷新二维码
func (p *QQPDPlugin) handleRefreshQRCode(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "用户不存在")
		return
	}

	// 强制生成新二维码
	qrcodeBytes, qrsig, err := p.generateQRCodeWithSig()
	if err != nil {
		respondError(c, "生成二维码失败: "+err.Error())
		return
	}

	// 缓存二维码
	user.QRCodeCache = qrcodeBytes
	user.QRCodeCacheTime = time.Now()
	user.Qrsig = qrsig

	qrcodeBase64 := "data:image/png;base64," + base64.StdEncoding.EncodeToString(qrcodeBytes)

	respondSuccess(c, "二维码已刷新", gin.H{
		"qrcode_base64": qrcodeBase64,
	})
}

// handleLogout 退出登录
func (p *QQPDPlugin) handleLogout(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "用户不存在")
		return
	}

	// 清除Cookie
	user.Cookie = ""
	user.Status = "pending"
	user.QQMasked = ""

	if err := p.saveUser(user); err != nil {
		respondError(c, "退出失败")
		return
	}

	if DebugLog {
		fmt.Printf("[QQPD] 用户 %s 已退出登录\n", hash[:8]+"...")
	}

	respondSuccess(c, "已退出登录", gin.H{
		"status": "pending",
	})
}

// handleCheckLogin 检查登录状态（前端轮询调用）
func (p *QQPDPlugin) handleCheckLogin(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "用户不存在")
		return
	}

	// 检查是否有qrsig
	if user.Qrsig == "" {
		respondError(c, "请先刷新二维码")
		return
	}

	// 检查登录状态
	loginResult, err := p.checkQRLoginStatus(user.Qrsig)
	if err != nil {
		respondError(c, err.Error())
		return
	}

	if loginResult.Status == "success" {
		// 登录成功，更新用户信息
		user.Cookie = loginResult.Cookie
		user.Status = "active"
		user.QQMasked = loginResult.QQMasked
		user.LoginAt = time.Now()
		// QQ Cookie的实际有效期通常是2天，设置为2天后过期（留一点缓冲时间）
		user.ExpireAt = time.Now().AddDate(0, 0, 2)

		if err := p.saveUser(user); err != nil {
			respondError(c, "保存失败: "+err.Error())
			return
		}

		if DebugLog {
			fmt.Printf("[QQPD] 用户 %s 登录成功，QQ: %s, Cookie包含keys: ", hash[:8]+"...", loginResult.QQMasked)
			// 打印Cookie中的所有key（不打印value保护隐私）
			cookies := parseCookieString(loginResult.Cookie)
			keys := make([]string, 0, len(cookies))
			for k := range cookies {
				keys = append(keys, k)
			}
			fmt.Printf("%v\n", keys)
		}

		respondSuccess(c, "登录成功", gin.H{
			"login_status": "success",
			"qq_masked":    loginResult.QQMasked,
		})
	} else if loginResult.Status == "waiting" {
		respondSuccess(c, "等待扫码", gin.H{
			"login_status": "waiting",
		})
	} else if loginResult.Status == "expired" {
		respondError(c, "二维码已失效，请刷新")
	} else {
		respondError(c, "登录检测失败")
	}
}

// handleManualLogin 手动登录（测试用）
func (p *QQPDPlugin) handleManualLogin(c *gin.Context, hash string, reqData map[string]interface{}) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "用户不存在")
		return
	}

	// 获取cookie和qq_masked参数
	cookie, _ := reqData["cookie"].(string)
	qqMasked, _ := reqData["qq_masked"].(string)

	if cookie == "" {
		respondError(c, "缺少cookie参数")
		return
	}

	// 测试Cookie有效性
	if !p.testCookieValid(cookie) {
		respondError(c, "Cookie无效或已失效")
		return
	}

	// 更新用户状态
	user.Cookie = cookie
	user.Status = "active"
	user.QQMasked = qqMasked
	user.LoginAt = time.Now()
	// QQ Cookie的实际有效期通常是2天，设置为2天后过期（留一点缓冲时间）
	user.ExpireAt = time.Now().AddDate(0, 0, 2)

	if err := p.saveUser(user); err != nil {
		respondError(c, "保存失败: "+err.Error())
		return
	}

	if DebugLog {
		fmt.Printf("[QQPD] 用户 %s 手动登录成功，QQ: %s, Cookie包含keys: ", hash[:8]+"...", qqMasked)
		cookies := parseCookieString(cookie)
		keys := make([]string, 0, len(cookies))
		for k := range cookies {
			keys = append(keys, k)
		}
		fmt.Printf("%v\n", keys)
	}

	respondSuccess(c, "登录成功", gin.H{
		"status":      "active",
		"qq_masked":   qqMasked,
		"login_time":  user.LoginAt.Format("2006-01-02 15:04:05"),
		"expire_time": user.ExpireAt.Format("2006-01-02 15:04:05"),
	})
}

// handleSetChannelsWithData 设置频道列表（覆盖式）
func (p *QQPDPlugin) handleSetChannelsWithData(c *gin.Context, hash string, reqData map[string]interface{}) {
	// 从reqData中提取channels字段
	channelsInterface, ok := reqData["channels"]
	if !ok {
		respondError(c, "缺少channels字段")
		return
	}

	// 转换为字符串数组
	channels := []string{}
	if channelsList, ok := channelsInterface.([]interface{}); ok {
		for _, ch := range channelsList {
			if chStr, ok := ch.(string); ok {
				channels = append(channels, chStr)
			}
		}
	}

	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "用户不存在")
		return
	}

	// 规范化频道列表（提取频道号，去重）
	normalizedChannels := []string{}
	seen := make(map[string]bool)
	invalid := []string{}

	for _, ch := range channels {
		normalized := p.normalizeChannel(ch)
		if normalized == "" {
			invalid = append(invalid, ch)
			continue
		}

		if !seen[normalized] {
			normalizedChannels = append(normalizedChannels, normalized)
			seen[normalized] = true
		}
	}

	p.mu.Lock()
	if user.ChannelGuildIDs == nil {
		user.ChannelGuildIDs = make(map[string]string)
	}
	for channelNumber, guildID := range user.ChannelGuildIDs {
		if !seen[channelNumber] || !validGuildID(guildID) {
			delete(user.ChannelGuildIDs, channelNumber)
		}
	}
	user.Channels = normalizedChannels
	user.LastAccessAt = time.Now()
	p.mu.Unlock()

	if err := p.saveUser(user); err != nil {
		respondError(c, "保存失败: "+err.Error())
		return
	}

	_, resolveErr := p.resolveChannelTasks(p.buildChannelTasks([]*User{user}))
	message := "频道列表已更新"
	warning := ""
	if resolveErr != nil {
		message += "，部分频道解析失败，将在搜索时重试"
		warning = resolveErr.Error()
	}
	p.mu.RLock()
	cachedCount := len(user.ChannelGuildIDs)
	p.mu.RUnlock()
	respondSuccess(c, message, gin.H{
		"channels":         normalizedChannels,
		"channel_count":    len(normalizedChannels),
		"invalid_channels": invalid,
		"guild_ids_cached": cachedCount,
		"warning":          warning,
	})
}

// handleTestSearchWithData 测试搜索
func (p *QQPDPlugin) handleTestSearchWithData(c *gin.Context, hash string, reqData map[string]interface{}) {
	// 提取参数
	keyword, ok := reqData["keyword"].(string)
	if !ok || keyword == "" {
		respondError(c, "缺少keyword字段")
		return
	}

	maxResults := 10
	if mr, ok := reqData["max_results"].(float64); ok && mr >= 1 {
		maxResults = int(mr)
	}

	user, exists := p.getUserByHash(hash)
	if !exists || user.Cookie == "" {
		respondError(c, "请先登录")
		return
	}

	if len(user.Channels) == 0 {
		respondError(c, "请先配置频道")
		return
	}

	tasks := p.buildChannelTasks([]*User{user})
	allResults, searchErr := p.executeTasks(tasks, keyword)
	warning := ""
	if searchErr != nil {
		if len(allResults) == 0 {
			respondError(c, searchErr.Error())
			return
		}
		warning = searchErr.Error()
	}

	// 不在插件内过滤，交给Service层处理
	// filteredResults := plugin.FilterResultsByKeyword(allResults, keyword)

	// 限制返回数量
	if len(allResults) > maxResults {
		allResults = allResults[:maxResults]
	}

	// 转换为前端需要的格式
	results := make([]gin.H, 0, len(allResults))
	for _, r := range allResults {
		links := make([]gin.H, 0, len(r.Links))
		for _, link := range r.Links {
			links = append(links, gin.H{
				"type":     link.Type,
				"url":      link.URL,
				"password": link.Password,
			})
		}

		results = append(results, gin.H{
			"unique_id": r.UniqueID, // 添加unique_id，显示来源频道
			"title":     r.Title,
			"links":     links,
		})
	}

	respondSuccess(c, fmt.Sprintf("找到 %d 条结果", len(results)), gin.H{
		"keyword":           keyword,
		"total_results":     len(results),
		"channels_searched": user.Channels,
		"results":           results,
		"warning":           warning,
	})
}

// ============ 搜索逻辑 ============

// buildChannelTasks 构建频道任务列表（去重+负载均衡）
func (p *QQPDPlugin) buildChannelTasks(users []*User) []ChannelTask {
	p.mu.RLock()
	defer p.mu.RUnlock()
	// 1. 收集所有频道及其所属用户
	channelOwners := make(map[string][]*User)

	for _, user := range users {
		for _, channelID := range user.Channels {
			channelOwners[channelID] = append(channelOwners[channelID], user)
		}
	}

	// 2. 为每个频道分配一个用户（负载均衡）
	tasks := []ChannelTask{}
	userTaskCount := make(map[string]int)

	for channelID, owners := range channelOwners {
		// 选择任务最少的用户来执行
		selectedUser := owners[0]
		minTasks := userTaskCount[selectedUser.Hash]

		for _, owner := range owners {
			if count := userTaskCount[owner.Hash]; count < minTasks {
				selectedUser = owner
				minTasks = count
			}
		}

		// 从缓存中获取guild_id（优先使用缓存）
		var guildID string
		if selectedUser.ChannelGuildIDs != nil {
			if cachedGuildID, exists := selectedUser.ChannelGuildIDs[channelID]; exists {
				guildID = cachedGuildID
				if DebugLog {
					fmt.Printf("[QQPD]   频道 %s: 使用缓存的guild_id %s\n", channelID, guildID)
				}
			}
		}

		if !validGuildID(guildID) {
			guildID = ""
		}

		// 创建任务
		tasks = append(tasks, ChannelTask{
			ChannelID: channelID,
			GuildID:   guildID,
			UserHash:  selectedUser.Hash,
			Cookie:    selectedUser.Cookie,
		})

		// 更新任务计数
		userTaskCount[selectedUser.Hash]++
	}

	return tasks
}

// executeTasks 并发执行所有频道搜索任务
func (p *QQPDPlugin) executeTasks(tasks []ChannelTask, keyword string) ([]model.SearchResult, error) {
	tasks, resolveErr := p.resolveChannelTasks(tasks)
	var allResults []model.SearchResult
	taskErrors := []error{resolveErr}
	var mu sync.Mutex
	var wg sync.WaitGroup

	// 使用信号量控制并发数
	semaphore := make(chan struct{}, MaxConcurrentChannels)

	for _, task := range tasks {
		wg.Add(1)
		go func(task ChannelTask) {
			defer wg.Done()

			// 获取信号量
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 搜索单个频道（使用预先获取的guild_id）
			results, err := p.searchSingleChannel(keyword, task.Cookie, task.ChannelID, task.GuildID)

			// 安全地追加结果（UniqueID已在extractResultInfo中设置）
			mu.Lock()
			allResults = append(allResults, results...)
			if err != nil {
				taskErrors = append(taskErrors, fmt.Errorf("频道 %s: %w", task.ChannelID, err))
			}
			mu.Unlock()
		}(task)
	}

	wg.Wait()
	return allResults, errors.Join(taskErrors...)
}

func (p *QQPDPlugin) searchSingleChannel(keyword, cookieStr, channelID, guildID string) ([]model.SearchResult, error) {
	if !validGuildID(guildID) {
		return nil, fmt.Errorf("[QQPD] 搜索需要有效的数字guild_id")
	}
	payload := map[string]interface{}{
		"guild_id":      guildID,
		"query":         keyword,
		"cookie":        "",
		"member_cookie": "",
		"search_type":   map[string]int{"type": 0, "feed_type": 0},
		"cond": map[string]interface{}{
			"channel_ids":    []string{},
			"feed_rank_type": 0,
			"type_list":      []int{2, 3},
		},
	}
	body, err := p.requestAPI(qqpdSearchEndpoint, cookieStr, `{"uint32_command":"0x9287","uint32_service_type":"2"}`, payload)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data *struct {
			UnionResult *struct {
				GuildFeeds []map[string]interface{} `json:"guild_feeds"`
			} `json:"union_result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("[QQPD] 解析搜索结果失败: %w", err)
	}
	if response.Data == nil {
		return nil, fmt.Errorf("[QQPD] 搜索响应缺少data字段")
	}
	results := make([]model.SearchResult, 0)
	if response.Data.UnionResult == nil {
		return results, nil
	}
	for index, item := range response.Data.UnionResult.GuildFeeds {
		result := p.extractResultInfo(item, channelID, index)
		if result.Title != "" && len(result.Links) > 0 {
			results = append(results, result)
		}
	}
	return results, nil
}

// extractResultInfo 从搜索结果中提取信息
func (p *QQPDPlugin) extractResultInfo(item map[string]interface{}, channelID string, index int) model.SearchResult {
	// 提取标题（去掉"名称："前缀，只取第一行）
	title, _ := item["title"].(string)
	if strings.HasPrefix(title, "名称：") {
		title = title[len("名称："):]
	}
	if idx := strings.Index(title, "\n"); idx > 0 {
		title = title[:idx]
	}
	title = strings.TrimSpace(title)

	// 从content提取网盘链接（不在插件层过滤，交给Service层处理）
	content, _ := item["content"].(string)
	links := p.extractLinksFromContent(content)

	// 提取时间戳（从create_time字段）
	datetime := time.Now() // 默认使用当前时间
	if createTimeStr, ok := item["create_time"].(string); ok && createTimeStr != "" {
		// create_time是Unix时间戳字符串，转换为int64
		if timestamp, err := strconv.ParseInt(createTimeStr, 10, 64); err == nil {
			datetime = time.Unix(timestamp, 0)
		}
	}

	// 提取图片URL列表
	var images []string
	if imagesInterface, ok := item["images"].([]interface{}); ok {
		for _, imgItem := range imagesInterface {
			if imgMap, ok := imgItem.(map[string]interface{}); ok {
				// 提取url字段
				if imgURL, ok := imgMap["url"].(string); ok && imgURL != "" {
					images = append(images, imgURL)
				}
			}
		}
	}

	return model.SearchResult{
		UniqueID: fmt.Sprintf("qqpd-%s-%d", channelID, index),
		Title:    title,
		Content:  content,
		Links:    links,
		Datetime: datetime,
		Images:   images,
		Channel:  "", // 插件搜索结果Channel必须为空
	}
}

// extractLinksFromContent 从内容中提取网盘链接（自动去重）
func (p *QQPDPlugin) extractLinksFromContent(content string) []model.Link {
	var links []model.Link
	seen := make(map[string]bool) // 用于去重

	// 定义网盘链接正则模式
	linkPatterns := []struct {
		pattern  string
		linkType string
	}{
		{`https://pan\.quark\.cn/s/[^\s\n]+`, "quark"},
		{`https://drive\.uc\.cn/s/[^\s\n]+`, "uc"},
		{`https://pan\.baidu\.com/s/[^\s\n?]+(?:\?pwd=[a-zA-Z0-9]+)?`, "baidu"},
		{`https://(?:aliyundrive\.com|www\.alipan\.com)/s/[^\s\n]+`, "aliyun"},
		{`https://pan\.xunlei\.com/s/[^\s\n]+`, "xunlei"},
		{`https://cloud\.189\.cn/(?:t|web/share)/[^\s\n]+`, "tianyi"},
		{`https://(?:115\.com|115cdn\.com)/s/[^\s\n?]+(?:\?password=[a-zA-Z0-9]+)?`, "115"},
		{`https://(?:123pan\.cn|www\.123912\.com|www\.123684\.com|www\.123685\.com|www\.123592\.com|www\.123pan\.com)/s/[^\s\n]+`, "123"},
		{`https://caiyun\.(?:139\.com|feixin\.10086\.cn)/[^\s\n]+`, "mobile"},
		{`https://mypikpak\.com/s/[^\s\n]+`, "pikpak"},
		{`magnet:\?xt=urn:btih:[^\n]+`, "magnet"},
		{`ed2k://\|file\|[^\n]+?\|/`, "ed2k"},
	}

	for _, lp := range linkPatterns {
		re := regexp.MustCompile(lp.pattern)
		matches := re.FindAllString(content, -1)

		for _, linkURL := range matches {
			// 去重检查（同一个URL只保留一次）
			if seen[linkURL] {
				continue
			}
			seen[linkURL] = true

			password := ""

			// 提取密码
			if strings.Contains(linkURL, "pwd=") {
				pwdRe := qqpdRe1
				if pwdMatch := pwdRe.FindStringSubmatch(linkURL); len(pwdMatch) > 1 {
					password = pwdMatch[1]
				}
			} else if strings.Contains(linkURL, "password=") {
				pwdRe := qqpdRe2
				if pwdMatch := pwdRe.FindStringSubmatch(linkURL); len(pwdMatch) > 1 {
					password = pwdMatch[1]
				}
			}

			links = append(links, model.Link{
				Type:     lp.linkType,
				URL:      linkURL,
				Password: password,
			})
		}
	}

	return links
}

// ============ QQ登录相关 ============

// LoginResult 登录检测结果
type LoginResult struct {
	Status   string // success/waiting/expired/error
	Cookie   string // 完整Cookie（登录成功时）
	QQMasked string // 脱敏QQ号
}

// checkQRLoginStatus 检查二维码登录状态（参考Python代码）
func (p *QQPDPlugin) checkQRLoginStatus(qrsig string) (*LoginResult, error) {
	// 计算ptqrtoken
	ptqrtoken := getptqrtoken(qrsig)

	// 登录检测URL
	loginCheckURL := fmt.Sprintf("https://xui.ptlogin2.qq.com/ssl/ptqrlogin?u1=https%%3A%%2F%%2Fpd.qq.com%%2Fexplore&ptqrtoken=%s&ptredirect=1&h=1&t=1&g=1&from_ui=1&ptlang=2052&action=0-0-1761211119400&js_ver=25100115&js_type=1&login_sig=&pt_uistyle=40&aid=1600001587&daid=823&&o1vId=11f3315cde61b7b5da200e4a09fe308c&pt_js_version=28d22679", ptqrtoken)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           util.ProxyFuncForTransport(),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	req, err := http.NewRequest("GET", loginCheckURL, nil)
	if err != nil {
		return nil, err
	}

	// 设置qrsig cookie
	req.AddCookie(&http.Cookie{Name: "qrsig", Value: qrsig})

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	bodyStr := string(body)

	// 检查登录状态
	if strings.Contains(bodyStr, "二维码已失效") {
		return &LoginResult{Status: "expired"}, nil
	}

	if strings.Contains(bodyStr, "登录成功") {
		// 提取ptsigx和uin
		ptsigx, uin, err := p.extractLoginInfo(bodyStr)
		if err != nil {
			fmt.Printf("[QQPD] 提取登录信息失败: %v, 响应: %s\n", err, bodyStr)
			return nil, fmt.Errorf("提取登录信息失败: %w", err)
		}

		// 获取完整Cookie（传递ptqrlogin返回的所有Set-Cookie）
		allSetCookies := resp.Header.Values("Set-Cookie")
		setCookieStr := strings.Join(allSetCookies, "; ")

		cookie, err := p.fetchFullCookie(uin, ptsigx, setCookieStr)
		if err != nil {
			fmt.Printf("[QQPD] 获取Cookie失败: %v\n", err)
			return nil, fmt.Errorf("获取Cookie失败: %w", err)
		}

		// 生成脱敏QQ号
		qqMasked := p.maskQQ(uin)

		if DebugLog {
			fmt.Printf("[QQPD] 登录成功！QQ: %s, Cookie长度: %d, 包含keys: ", qqMasked, len(cookie))
			cookies := parseCookieString(cookie)
			keys := make([]string, 0, len(cookies))
			for k := range cookies {
				keys = append(keys, k)
			}
			fmt.Printf("%v\n", keys)
		}

		return &LoginResult{
			Status:   "success",
			Cookie:   cookie,
			QQMasked: qqMasked,
		}, nil
	}

	// 等待扫码
	return &LoginResult{Status: "waiting"}, nil
}

// extractLoginInfo 从登录响应中提取ptsigx和uin
func (p *QQPDPlugin) extractLoginInfo(responseText string) (string, string, error) {
	// 解析返回的JavaScript回调：ptuiCB('0','0','url',...)
	// 需要提取第3个参数的URL
	start := strings.Index(responseText, "ptuiCB(")
	if start == -1 {
		return "", "", fmt.Errorf("未找到ptuiCB")
	}

	// 简单解析，提取URL部分
	re := qqpdRe3
	matches := re.FindStringSubmatch(responseText)
	if len(matches) < 2 {
		return "", "", fmt.Errorf("无法解析响应")
	}

	url := matches[1]

	// 提取ptsigx
	ptsigxRe := qqpdRe4
	ptsigxMatches := ptsigxRe.FindStringSubmatch(url)
	if len(ptsigxMatches) < 2 {
		return "", "", fmt.Errorf("未找到ptsigx")
	}
	ptsigx := ptsigxMatches[1]

	// 提取uin
	uinRe := qqpdRe5
	uinMatches := uinRe.FindStringSubmatch(url)
	if len(uinMatches) < 2 {
		return "", "", fmt.Errorf("未找到uin")
	}
	uin := uinMatches[1]

	return ptsigx, uin, nil
}

// fetchFullCookie 获取完整Cookie
func (p *QQPDPlugin) fetchFullCookie(uin, ptsigx, setCookieHeader string) (string, error) {
	checkSigURL := fmt.Sprintf("https://ptlogin2.pd.qq.com/check_sig?pttype=1&uin=%s&service=ptqrlogin&nodirect=1&ptsigx=%s&s_url=https%%3A%%2F%%2Fpd.qq.com%%2Fexplore&f_url=&ptlang=2052&ptredirect=101&aid=1600001587&daid=823&j_later=0&low_login_hour=0&regmaster=0&pt_login_type=3&pt_aid=0&pt_aaid=16&pt_light=0&pt_3rd_aid=0", uin, ptsigx)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           util.ProxyFuncForTransport(),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	req, err := http.NewRequest("GET", checkSigURL, nil)
	if err != nil {
		return "", err
	}

	// 设置Cookie头
	req.Header.Set("Cookie", setCookieHeader)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	// 优先使用resp.Cookies()获取cookies（Go的http.Client自动解析Set-Cookie）
	cookieDict := make(map[string]string)

	// 首先从resp.Cookies()获取（更可靠，自动处理Set-Cookie）
	for _, cookie := range resp.Cookies() {
		if cookie.Value != "" {
			cookieDict[cookie.Name] = cookie.Value
		}
	}

	// 补充从Set-Cookie头解析（处理resp.Cookies()可能遗漏的cookies）
	allSetCookies := resp.Header.Values("Set-Cookie")
	for _, setCookie := range allSetCookies {
		// 解析Set-Cookie头：只提取cookie名称和值，忽略属性
		cookieName, cookieValue := p.parseSetCookieHeader(setCookie)
		if cookieName != "" && cookieValue != "" {
			// 如果resp.Cookies()中没有，则添加
			if _, exists := cookieDict[cookieName]; !exists {
				cookieDict[cookieName] = cookieValue
			}
		}
	}

	// 手动添加uin（加上o0前缀）
	if _, exists := cookieDict["uin"]; !exists || !strings.HasPrefix(cookieDict["uin"], "o") {
		cookieDict["uin"] = "o0" + uin
	}

	// 转换为Cookie字符串
	var cookiePairs []string
	for k, v := range cookieDict {
		cookiePairs = append(cookiePairs, fmt.Sprintf("%s=%s", k, v))
	}

	return strings.Join(cookiePairs, "; "), nil
}

// parseSetCookieHeader 从Set-Cookie响应头中解析cookie（只提取名称和值，忽略属性）
func (p *QQPDPlugin) parseSetCookieHeader(setCookie string) (string, string) {
	// Set-Cookie格式: "name=value; Path=/; Domain=.qq.com; ..."
	// 只取第一个分号之前的部分
	parts := strings.Split(setCookie, ";")
	if len(parts) == 0 {
		return "", ""
	}

	nameValue := strings.TrimSpace(parts[0])
	idx := strings.Index(nameValue, "=")
	if idx <= 0 {
		return "", ""
	}

	key := strings.TrimSpace(nameValue[:idx])
	value := strings.TrimSpace(nameValue[idx+1:])

	// 跳过cookie属性（不是真正的cookie名称）
	skipAttrs := map[string]bool{
		"Domain": true, "Path": true, "Expires": true, "Max-Age": true,
		"SameSite": true, "Secure": true, "HttpOnly": true,
	}
	if skipAttrs[key] {
		return "", ""
	}

	return key, value
}

// refreshCookie 刷新cookies（更新uuid等动态字段）
func (p *QQPDPlugin) refreshCookie(cookieStr string) string {
	if cookieStr == "" {
		return cookieStr
	}

	// 解析现有cookies
	oldCookies := parseCookieString(cookieStr)
	uin := oldCookies["uin"]
	if uin == "" {
		return cookieStr
	}

	// 去掉o0前缀
	if strings.HasPrefix(uin, "o0") {
		uin = uin[2:]
	} else if strings.HasPrefix(uin, "o") {
		uin = uin[1:]
	}

	// 访问pd.qq.com获取新的cookies（主要是uuid）
	pdURL := "https://pd.qq.com/explore"
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:           util.ProxyFuncForTransport(),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	req, err := http.NewRequest("GET", pdURL, nil)
	if err != nil {
		return cookieStr
	}

	req.Header.Set("Cookie", cookieStr)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return cookieStr
	}
	defer resp.Body.Close()

	// 从响应中提取新cookies
	newCookies := make(map[string]string)

	// 优先使用resp.Cookies()
	for _, cookie := range resp.Cookies() {
		if cookie.Value != "" {
			newCookies[cookie.Name] = cookie.Value
		}
	}

	// 补充从Set-Cookie头解析
	for _, setCookie := range resp.Header.Values("Set-Cookie") {
		key, value := p.parseSetCookieHeader(setCookie)
		if key != "" && value != "" {
			if _, exists := newCookies[key]; !exists {
				newCookies[key] = value
			}
		}
	}

	// 如果有新cookies，合并更新
	if len(newCookies) > 0 {
		mergedCookies := make(map[string]string)
		// 先复制旧的
		for k, v := range oldCookies {
			mergedCookies[k] = v
		}
		// 用新的覆盖
		for k, v := range newCookies {
			mergedCookies[k] = v
		}

		// 确保uin格式正确
		if uinRaw, exists := mergedCookies["uin"]; !exists || !strings.HasPrefix(uinRaw, "o") {
			mergedCookies["uin"] = "o0" + uin
		}

		// 转换为Cookie字符串
		var cookiePairs []string
		for k, v := range mergedCookies {
			cookiePairs = append(cookiePairs, fmt.Sprintf("%s=%s", k, v))
		}

		return strings.Join(cookiePairs, "; ")
	}

	return cookieStr
}

// maskQQ 生成脱敏QQ号
func (p *QQPDPlugin) maskQQ(uin string) string {
	if len(uin) <= 4 {
		return uin
	}
	// 前4位 + **** + 后2位
	if len(uin) > 6 {
		return uin[:4] + "****" + uin[len(uin)-2:]
	}
	return uin[:2] + "****" + uin[len(uin)-2:]
}

// generateQRCodeWithSig 生成QQ登录二维码并返回qrsig
func (p *QQPDPlugin) generateQRCodeWithSig() ([]byte, string, error) {
	qrcodeURL := "https://xui.ptlogin2.qq.com/ssl/ptqrshow?appid=1600001587&e=2&l=M&s=3&d=72&v=4&t=0.3680011491059967&daid=823&pt_3rd_aid=0"

	client := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy:           util.ProxyFuncForTransport(),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	resp, err := client.Get(qrcodeURL)
	if err != nil {
		return nil, "", fmt.Errorf("请求二维码失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("二维码请求返回状态码: %d", resp.StatusCode)
	}

	// 读取二维码图片
	qrcodeBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("读取二维码失败: %w", err)
	}

	// 提取qrsig（用于后续登录检测）
	setCookie := resp.Header.Get("Set-Cookie")
	qrsig := extractQrsig(setCookie)
	if qrsig != "" && DebugLog {
		fmt.Printf("[QQPD] 二维码生成成功，qrsig: %s\n", qrsig[:20]+"...")
	}

	return qrcodeBytes, qrsig, nil
}

// extractQrsig 从Set-Cookie中提取qrsig
func extractQrsig(setCookie string) string {
	cookies := strings.Split(setCookie, ";")
	for _, cookie := range cookies {
		cookie = strings.TrimSpace(cookie)
		if strings.HasPrefix(cookie, "qrsig=") {
			return strings.TrimPrefix(cookie, "qrsig=")
		}
	}
	return ""
}

// getptqrtoken 计算ptqrtoken
func getptqrtoken(qrsig string) string {
	e := 0
	for i := 1; i <= len(qrsig); i++ {
		e += (e << 5) + int(qrsig[i-1])
	}
	return fmt.Sprintf("%d", 2147483647&e)
}

// bkn 计算bkn值
func bkn(skey string) int64 {
	t, n, o := int64(5381), 0, len(skey)
	for n < o {
		t += (t << 5) + int64(skey[n])
		n++
	}
	return t & 2147483647
}

// testCookieValid 测试Cookie是否有效
func (p *QQPDPlugin) testCookieValid(cookieStr string) bool {
	// 测试前刷新cookies（更新uuid等动态字段）
	cookieStr = p.refreshCookie(cookieStr)

	// 解析cookie获取p_skey
	cookies := parseCookieString(cookieStr)
	pSkey, ok := cookies["p_skey"]
	if !ok || pSkey == "" {
		return false
	}

	// 计算bkn
	bknValue := bkn(pSkey)

	// 尝试一个简单的请求测试
	testURL := fmt.Sprintf("https://pd.qq.com/qunng/guild/gotrpc/auth/trpc.group_pro.in_guild_search_svr.InGuildSearch/NewSearch?bkn=%d", bknValue)

	headers := map[string]string{
		"x-oidb":       `{"uint32_command":"0x9287","uint32_service_type":"2"}`,
		"content-type": "application/json",
	}

	payload := map[string]interface{}{
		"guild_id":      "592843764045681811",
		"query":         "test",
		"cookie":        "",
		"member_cookie": "",
		"search_type":   map[string]int{"type": 0, "feed_type": 0},
		"cond":          map[string]interface{}{"channel_ids": []string{}, "feed_rank_type": 0, "type_list": []int{2, 3}},
	}

	client := &http.Client{Timeout: 10 * time.Second}
	payloadBytes, _ := json.Marshal(payload)

	req, err := http.NewRequest("POST", testURL, strings.NewReader(string(payloadBytes)))
	if err != nil {
		return false
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	// 设置Cookie
	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		var result map[string]interface{}
		body, _ := ioutil.ReadAll(resp.Body)
		if err := json.Unmarshal(body, &result); err == nil {
			if retcode, ok := result["retcode"].(float64); ok && retcode == 0 {
				return true
			}
			if _, hasData := result["data"]; hasData {
				return true
			}
		}
	}

	return false
}

// parseCookieString 解析Cookie字符串为map（用于读取保存的cookie文件）
func parseCookieString(cookieStr string) map[string]string {
	cookies := make(map[string]string)
	if cookieStr == "" {
		return cookies
	}

	pairs := strings.Split(cookieStr, ";")
	skipAttrs := map[string]bool{
		"Domain": true, "Path": true, "Expires": true, "Max-Age": true,
		"SameSite": true, "Secure": true, "HttpOnly": true,
	}

	for _, pair := range pairs {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if idx := strings.Index(pair, "="); idx > 0 {
			key := strings.TrimSpace(pair[:idx])
			value := strings.TrimSpace(pair[idx+1:])
			// 跳过cookie属性（只保留真正的cookie名称）
			if key != "" && value != "" && !skipAttrs[key] {
				cookies[key] = value
			}
		}
	}

	return cookies
}

// ============ 工具函数 ============

// generateHash hash生成函数（完整hash，不截取）
func (p *QQPDPlugin) generateHash(qq string) string {
	salt := os.Getenv("QQPD_HASH_SALT")
	if salt == "" {
		salt = "pansou_qqpd_secret_2025"
	}
	data := qq + salt
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// normalizeChannel 从URL或纯文本中提取频道号
func (p *QQPDPlugin) normalizeChannel(input string) string {
	input = strings.TrimSpace(input)

	// 如果是URL格式: https://pd.qq.com/g/pd97631607
	if strings.Contains(input, "pd.qq.com/g/") {
		parts := strings.Split(input, "/g/")
		if len(parts) == 2 {
			return strings.TrimSpace(parts[1])
		}
	}

	// 直接返回（假设是频道号）
	return input
}

// isHexString 判断字符串是否为十六进制
func (p *QQPDPlugin) isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// respondSuccess 成功响应
func respondSuccess(c *gin.Context, message string, data interface{}) {
	c.JSON(200, gin.H{
		"success": true,
		"message": message,
		"data":    data,
	})
}

// respondError 错误响应
func respondError(c *gin.Context, message string) {
	c.JSON(200, gin.H{
		"success": false,
		"message": message,
		"data":    nil,
	})
}

// ============ Cookie加密 ============

// getEncryptionKey 获取加密密钥
func getEncryptionKey() []byte {
	key := os.Getenv("QQPD_ENCRYPTION_KEY")
	if key == "" {
		key = "default-32-byte-key-change-me!" // 32字节
	}
	return []byte(key)[:32]
}

// encryptCookie 加密Cookie
func encryptCookie(plaintext string) (string, error) {
	key := getEncryptionKey()

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decryptCookie 解密Cookie
func decryptCookie(encrypted string) (string, error) {
	key := getEncryptionKey()

	ciphertext, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}

// ============ 定期清理 ============

// startCleanupTask 定期清理任务
func (p *QQPDPlugin) startCleanupTask() {
	ticker := time.NewTicker(24 * time.Hour)
	for range ticker.C {
		deleted := p.cleanupExpiredUsers()
		marked := p.markInactiveUsers()

		if deleted > 0 || marked > 0 {
			fmt.Printf("[QQPD] 清理任务完成: 删除 %d 个过期用户, 标记 %d 个不活跃用户\n", deleted, marked)
		}
	}
}

// cleanupExpiredUsers 清理过期用户（从内存和文件）
func (p *QQPDPlugin) cleanupExpiredUsers() int {
	deletedCount := 0
	now := time.Now()
	expireThreshold := now.AddDate(0, 0, -30) // 30天前

	// 遍历内存中的用户
	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)

		// 删除条件：状态为expired且超过30天未访问
		if user.Status == "expired" && user.LastAccessAt.Before(expireThreshold) {
			if err := p.deleteUser(user.Hash); err == nil {
				deletedCount++
			}
		}
		return true
	})

	return deletedCount
}

// markInactiveUsers 标记长期未使用的用户为过期
func (p *QQPDPlugin) markInactiveUsers() int {
	markedCount := 0
	now := time.Now()
	inactiveThreshold := now.AddDate(0, 0, -90) // 90天前

	// 遍历内存中的用户
	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)

		// 标记条件：超过90天未访问
		if user.LastAccessAt.Before(inactiveThreshold) && user.Status != "expired" {
			user.Status = "expired"
			user.Cookie = "" // 清空Cookie

			// 更新内存和文件
			if err := p.saveUser(user); err == nil {
				markedCount++
			}
		}
		return true
	})

	return markedCount
}

// 以下正则原先在函数内临时编译，每次调用都要重新解析模式；
// 提到包级后只编译一次，匹配行为不变。
var (
	qqpdRe1 = regexp.MustCompile(`pwd=([a-zA-Z0-9]+)`)
	qqpdRe2 = regexp.MustCompile(`password=([a-zA-Z0-9]+)`)
	qqpdRe3 = regexp.MustCompile(`ptuiCB\('0','0','([^']+)'`)
	qqpdRe4 = regexp.MustCompile(`ptsigx=([A-Za-z0-9]+)`)
	qqpdRe5 = regexp.MustCompile(`uin=(\d+)`)
)
