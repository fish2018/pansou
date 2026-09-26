package weibo

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"pansou/util"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"

	"github.com/gin-gonic/gin"
)

const (
	MaxConcurrentUsers = 10 // 最多同时搜索多少个微博用户
	MaxConcurrentWeibo = 30 // 最多同时处理多少条微博（获取评论）
	MaxComments        = 1  // 每条微博最多获取多少条评论
	DebugLog           = false
)

var StorageDir string

const HTMLTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>PanSou 微博搜索配置</title>
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
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>🔍 PanSou 微博搜索</h1>
            <p>配置你的专属搜索服务</p>
            <p style="font-size: 12px; margin-top: 10px; opacity: 0.8;">
                🔗 当前地址: <span id="current-url">HASH_PLACEHOLDER</span>
            </p>
        </div>

        <div class="section" id="login-section">
            <div class="section-title">📱 登录状态</div>
            
            <div id="logged-in-view" class="hidden">
                <div class="status-box">
                    <div class="status-item">
                        <span>状态</span>
                        <span><strong style="color: #48bb78;">✅ 已登录</strong></span>
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
                        请使用手机微博扫描二维码登录
                    </p>
                    <p style="font-size: 12px; color: #999;">扫码后自动检测登录状态</p>
                    <button class="btn btn-secondary" onclick="refreshQRCode()" style="margin-top: 10px;">
                        刷新二维码
                    </button>
                </div>
            </div>
        </div>

        <div class="section" id="users-section">
            <div class="section-title">👤 微博用户管理 (<span id="user-count">0</span> 个)</div>
            
            <div id="alert-box"></div>
            
            <p style="margin-bottom: 10px; color: #666;">每行一个微博用户ID，保存时自动去重</p>
            <textarea id="users-textarea" rows="10" placeholder="5487050770
1234567890
9876543210"></textarea>
            
            <button class="btn btn-primary" onclick="saveUsers()" style="margin-top: 10px;">保存用户配置</button>
        </div>

        <div class="section" id="test-section">
            <div class="section-title">🔍 测试搜索(限制返回10条数据)</div>
            
            <div style="display: flex; gap: 10px;">
                <input type="text" id="search-keyword" placeholder="输入关键词测试搜索" style="flex: 1; padding: 10px; border: 1px solid #ddd; border-radius: 6px;">
                <button class="btn btn-primary" onclick="testSearch()">搜索</button>
            </div>

            <div id="search-results" class="test-results hidden"></div>
        </div>
    </div>

    <script>
        const HASH = 'HASH_PLACEHOLDER';
        const API_URL = '/weibo/' + HASH;
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
            if (loginCheckInterval) return;
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
                    document.getElementById('logged-in-view').classList.remove('hidden');
                    document.getElementById('not-logged-in-view').classList.add('hidden');
                    
                    document.getElementById('login-time').textContent = data.login_time || '-';
                    document.getElementById('expire-info').textContent = '剩余 ' + (data.expires_in_days || 0) + ' 天';
                    
                    stopLoginPolling();
                } else {
                    document.getElementById('logged-in-view').classList.add('hidden');
                    document.getElementById('not-logged-in-view').classList.remove('hidden');
                    
                    if (data.qrcode_base64) {
                        document.getElementById('qrcode-img').src = data.qrcode_base64;
                    }
                    
                    startLoginPolling();
                }

                updateUserList(data.user_ids || []);
            }
        }

        async function checkLogin() {
            const result = await postAction('check_login');
            if (result.success && result.data) {
                if (result.data.login_status === 'success') {
                    stopLoginPolling();
                    showAlert('登录成功！');
                    updateStatus();
                }
            }
        }

        function updateUserList(userIds) {
            const textarea = document.getElementById('users-textarea');
            const count = document.getElementById('user-count');
            
            count.textContent = userIds.length;
            
            if (document.activeElement !== textarea) {
                textarea.value = userIds.join('\n');
            }
        }

        function showAlert(message, type = 'success') {
            const alertBox = document.getElementById('alert-box');
            alertBox.innerHTML = '<div class="alert alert-' + type + '">' + message + '</div>';
            setTimeout(() => {
                alertBox.innerHTML = '';
            }, 3000);
        }

        async function refreshQRCode() {
            const result = await postAction('refresh_qrcode');
            if (result.success) {
                showAlert(result.message);
                updateStatus();
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

        async function saveUsers() {
            const textarea = document.getElementById('users-textarea');
            const usersText = textarea.value.trim();
            
            const userIds = usersText
                .split('\n')
                .map(line => line.trim())
                .filter(line => line.length > 0);
            
            const result = await postAction('set_user_ids', { user_ids: userIds });
            if (result.success) {
                showAlert(result.message);
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
                resultsDiv.innerHTML = '<p style="color: red;">' + result.message + '</p>';
            }
        }

        document.getElementById('search-keyword').addEventListener('keypress', function(e) {
            if (e.key === 'Enter') testSearch();
        });
    </script>
</body>
</html>`

type WeiboPlugin struct {
	*plugin.BaseAsyncPlugin
	users       sync.Map
	mu          sync.RWMutex
	initialized bool
}

type User struct {
	Hash         string    `json:"hash"`
	Cookie       string    `json:"cookie"`
	Status       string    `json:"status"`
	UserIDs      []string  `json:"user_ids"`
	CreatedAt    time.Time `json:"created_at"`
	LoginAt      time.Time `json:"login_at"`
	ExpireAt     time.Time `json:"expire_at"`
	LastAccessAt time.Time `json:"last_access_at"`
	LastRefresh  time.Time `json:"last_refresh"` // Cookie上次刷新时间

	QRCodeCache     []byte    `json:"-"`
	QRCodeCacheTime time.Time `json:"-"`
	Qrsig           string    `json:"-"`
}

type UserTask struct {
	UserID string
	Cookie string
}

func init() {
	p := &WeiboPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("weibo", 3),
	}

	plugin.RegisterGlobalPlugin(p)
}

// Initialize 实现 InitializablePlugin 接口，延迟初始化插件
func (p *WeiboPlugin) Initialize() error {
	if p.initialized {
		return nil
	}

	// 初始化存储目录路径
	cachePath := os.Getenv("CACHE_PATH")
	if cachePath == "" {
		cachePath = "./cache"
	}
	StorageDir = filepath.Join(cachePath, "weibo_users")

	if err := os.MkdirAll(StorageDir, 0755); err != nil {
		return fmt.Errorf("创建存储目录失败: %v", err)
	}

	p.loadAllUsers()
	go p.startCleanupTask()

	p.initialized = true
	return nil
}

func (p *WeiboPlugin) RegisterWebRoutes(router *gin.RouterGroup) {
	weibo := router.Group("/weibo")
	weibo.GET("/:param", p.handleManagePage)
	weibo.POST("/:param", p.handleManagePagePOST)

	fmt.Printf("[Weibo] Web路由已注册: /weibo/:param\n")
}

func (p *WeiboPlugin) SkipServiceFilter() bool {
	// 微博插件已经在API层面过滤了关键词，不需要Service层再次过滤
	return true
}

func (p *WeiboPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

func (p *WeiboPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	if DebugLog {
		fmt.Printf("[Weibo] ========== 开始搜索: %s ==========\n", keyword)
	}

	users := p.getActiveUsers()
	if DebugLog {
		fmt.Printf("[Weibo] 找到 %d 个有效用户\n", len(users))
	}

	if len(users) == 0 {
		if DebugLog {
			fmt.Printf("[Weibo] 没有有效用户，返回空结果\n")
		}
		return model.PluginSearchResult{Results: []model.SearchResult{}, IsFinal: true}, nil
	}

	if len(users) > MaxConcurrentUsers {
		sort.Slice(users, func(i, j int) bool {
			return users[i].LastAccessAt.After(users[j].LastAccessAt)
		})
		users = users[:MaxConcurrentUsers]
	}

	tasks := p.buildUserTasks(users)
	results := p.executeTasks(tasks, keyword)

	if DebugLog {
		fmt.Printf("[Weibo] 搜索完成，返回 %d 条结果\n", len(results))
	}

	return model.PluginSearchResult{
		Results: results,
		IsFinal: true,
		Source:  "plugin:weibo",
	}, nil
}

func (p *WeiboPlugin) loadAllUsers() {
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

		p.users.Store(user.Hash, &user)
		count++
	}

	fmt.Printf("[Weibo] 已加载 %d 个用户到内存\n", count)
}

func (p *WeiboPlugin) getUserByHash(hash string) (*User, bool) {
	value, ok := p.users.Load(hash)
	if !ok {
		return nil, false
	}
	return value.(*User), true
}

func (p *WeiboPlugin) saveUser(user *User) error {
	p.users.Store(user.Hash, user)
	return p.persistUser(user)
}

func (p *WeiboPlugin) persistUser(user *User) error {
	filePath := filepath.Join(StorageDir, user.Hash+".json")
	data, err := json.MarshalIndent(user, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filePath, data, 0644)
}

func (p *WeiboPlugin) deleteUser(hash string) error {
	p.users.Delete(hash)
	filePath := filepath.Join(StorageDir, hash+".json")
	return os.Remove(filePath)
}

func (p *WeiboPlugin) getActiveUsers() []*User {
	var users []*User

	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)

		if user.Status != "active" {
			return true
		}

		if !user.ExpireAt.IsZero() && time.Now().After(user.ExpireAt) {
			user.Status = "expired"
			user.Cookie = ""
			p.saveUser(user)
			return true
		}

		if len(user.UserIDs) == 0 {
			return true
		}

		users = append(users, user)
		return true
	})

	return users
}

func (p *WeiboPlugin) handleManagePage(c *gin.Context) {
	param := c.Param("param")

	if len(param) == 64 && p.isHexString(param) {
		html := strings.ReplaceAll(HTMLTemplate, "HASH_PLACEHOLDER", param)
		c.Data(200, "text/html; charset=utf-8", []byte(html))
	} else {
		hash := p.generateHash(param)
		c.Redirect(302, "/weibo/"+hash)
	}
}

func (p *WeiboPlugin) handleManagePagePOST(c *gin.Context) {
	hash := c.Param("param")

	var reqData map[string]interface{}
	if err := c.ShouldBindJSON(&reqData); err != nil {
		respondError(c, "无效的请求格式: "+err.Error())
		return
	}

	action, ok := reqData["action"].(string)
	if !ok || action == "" {
		respondError(c, "缺少action字段")
		return
	}

	switch action {
	case "get_status":
		p.handleGetStatus(c, hash)
	case "refresh_qrcode":
		p.handleRefreshQRCode(c, hash)
	case "logout":
		p.handleLogout(c, hash)
	case "set_user_ids":
		p.handleSetUserIDs(c, hash, reqData)
	case "test_search":
		p.handleTestSearch(c, hash, reqData)
	case "check_login":
		p.handleCheckLogin(c, hash)
	default:
		respondError(c, "未知的操作类型: "+action)
	}
}

func (p *WeiboPlugin) handleGetStatus(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)

	if !exists {
		user = &User{
			Hash:         hash,
			Status:       "pending",
			UserIDs:      []string{},
			CreatedAt:    time.Now(),
			LastAccessAt: time.Now(),
		}
		p.saveUser(user)
	} else {
		user.LastAccessAt = time.Now()
		p.saveUser(user)
	}

	loggedIn := false
	if user.Status == "active" && user.Cookie != "" {
		loggedIn = true
	}

	fmt.Printf("[Weibo DEBUG] handleGetStatus - hash: %s, Status: %s, Cookie长度: %d, loggedIn: %v\n",
		hash, user.Status, len(user.Cookie), loggedIn)

	var qrcodeBase64 string
	if !loggedIn {
		if user.QRCodeCache != nil && time.Since(user.QRCodeCacheTime) < 30*time.Second {
			qrcodeBase64 = "data:image/png;base64," + base64.StdEncoding.EncodeToString(user.QRCodeCache)
		} else {
			qrcodeBytes, qrsig, err := p.generateQRCodeWithSig()
			if err == nil {
				qrcodeBase64 = "data:image/png;base64," + base64.StdEncoding.EncodeToString(qrcodeBytes)
				user.QRCodeCache = qrcodeBytes
				user.QRCodeCacheTime = time.Now()
				user.Qrsig = qrsig
				p.saveUser(user)
			}
		}
	}

	expiresInDays := 0
	if !user.ExpireAt.IsZero() {
		expiresInDays = int(time.Until(user.ExpireAt).Hours() / 24)
		if expiresInDays < 0 {
			expiresInDays = 0
		}
	}

	responseData := gin.H{
		"hash":            hash,
		"logged_in":       loggedIn,
		"status":          user.Status,
		"login_time":      user.LoginAt.Format("2006-01-02 15:04:05"),
		"expire_time":     user.ExpireAt.Format("2006-01-02 15:04:05"),
		"expires_in_days": expiresInDays,
		"user_ids":        user.UserIDs,
		"qrcode_base64":   qrcodeBase64,
	}

	fmt.Printf("[Weibo DEBUG] handleGetStatus响应 - logged_in: %v, status: %s\n", loggedIn, user.Status)

	respondSuccess(c, "获取成功", responseData)
}

func (p *WeiboPlugin) handleRefreshQRCode(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "用户不存在")
		return
	}

	qrcodeBytes, qrsig, err := p.generateQRCodeWithSig()
	if err != nil {
		respondError(c, "生成二维码失败: "+err.Error())
		return
	}

	user.QRCodeCache = qrcodeBytes
	user.QRCodeCacheTime = time.Now()
	user.Qrsig = qrsig
	p.saveUser(user)

	qrcodeBase64 := "data:image/png;base64," + base64.StdEncoding.EncodeToString(qrcodeBytes)

	respondSuccess(c, "二维码已刷新", gin.H{
		"qrcode_base64": qrcodeBase64,
	})
}

func (p *WeiboPlugin) handleLogout(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "用户不存在")
		return
	}

	user.Cookie = ""
	user.Status = "pending"

	if err := p.saveUser(user); err != nil {
		respondError(c, "退出失败")
		return
	}

	respondSuccess(c, "已退出登录", gin.H{
		"status": "pending",
	})
}

func (p *WeiboPlugin) handleCheckLogin(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "用户不存在")
		return
	}

	if user.Qrsig == "" {
		respondError(c, "请先刷新二维码")
		return
	}

	loginResult, err := p.checkQRLoginStatus(user.Qrsig)
	if err != nil {
		fmt.Printf("[Weibo] checkQRLoginStatus错误: %v\n", err)
		respondError(c, err.Error())
		return
	}

	fmt.Printf("[Weibo] checkQRLoginStatus返回状态: %s, Cookie长度: %d\n", loginResult.Status, len(loginResult.Cookie))

	if loginResult.Status == "success" {
		fmt.Printf("[Weibo DEBUG] 登录成功! 开始更新用户状态...\n")

		user.Cookie = loginResult.Cookie
		user.Status = "active"
		user.LoginAt = time.Now()
		user.ExpireAt = time.Now().AddDate(0, 0, 30)
		user.Qrsig = ""
		user.QRCodeCache = nil

		fmt.Printf("[Weibo DEBUG] 更新后 - Status: %s, Cookie长度: %d\n", user.Status, len(user.Cookie))

		// 保存到内存和文件
		p.users.Store(hash, user)
		fmt.Printf("[Weibo DEBUG] 已保存到内存\n")

		if err := p.persistUser(user); err != nil {
			fmt.Printf("[Weibo DEBUG] 持久化失败: %v\n", err)
			respondError(c, "保存失败: "+err.Error())
			return
		}
		fmt.Printf("[Weibo DEBUG] 已持久化到文件\n")

		respondSuccess(c, "登录成功", gin.H{
			"login_status": "success",
		})
		fmt.Printf("[Weibo DEBUG] 已返回成功响应\n")
	} else if loginResult.Status == "waiting" {
		respondSuccess(c, "等待扫码", gin.H{
			"login_status": "waiting",
		})
	} else if loginResult.Status == "expired" {
		respondError(c, "二维码已失效，请刷新")
	} else {
		respondSuccess(c, "等待扫码", gin.H{
			"login_status": "waiting",
		})
	}
}

func (p *WeiboPlugin) handleSetUserIDs(c *gin.Context, hash string, reqData map[string]interface{}) {
	userIDsInterface, ok := reqData["user_ids"]
	if !ok {
		respondError(c, "缺少user_ids字段")
		return
	}

	userIDs := []string{}
	if userIDsList, ok := userIDsInterface.([]interface{}); ok {
		for _, uid := range userIDsList {
			if uidStr, ok := uid.(string); ok {
				userIDs = append(userIDs, uidStr)
			}
		}
	}

	user, exists := p.getUserByHash(hash)
	if !exists {
		respondError(c, "用户不存在")
		return
	}

	normalizedUserIDs := []string{}
	seen := make(map[string]bool)

	for _, uid := range userIDs {
		uid = strings.TrimSpace(uid)
		if uid == "" {
			continue
		}
		if !seen[uid] {
			normalizedUserIDs = append(normalizedUserIDs, uid)
			seen[uid] = true
		}
	}

	user.UserIDs = normalizedUserIDs
	user.LastAccessAt = time.Now()

	if err := p.saveUser(user); err != nil {
		respondError(c, "保存失败: "+err.Error())
		return
	}

	respondSuccess(c, "用户列表已更新", gin.H{
		"user_ids":   normalizedUserIDs,
		"user_count": len(normalizedUserIDs),
	})
}

func (p *WeiboPlugin) handleTestSearch(c *gin.Context, hash string, reqData map[string]interface{}) {
	keyword, ok := reqData["keyword"].(string)
	if !ok || keyword == "" {
		respondError(c, "缺少keyword字段")
		return
	}

	user, exists := p.getUserByHash(hash)
	if !exists || user.Cookie == "" {
		respondError(c, "请先登录")
		return
	}

	if len(user.UserIDs) == 0 {
		respondError(c, "请先配置微博用户ID")
		return
	}

	tasks := []UserTask{}
	for _, uid := range user.UserIDs {
		tasks = append(tasks, UserTask{
			UserID: uid,
			Cookie: user.Cookie,
		})
	}

	allResults := p.executeTasks(tasks, keyword)

	maxResults := 10
	if len(allResults) > maxResults {
		allResults = allResults[:maxResults]
	}

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
			"unique_id": r.UniqueID,
			"title":     r.Title,
			"links":     links,
		})
	}

	respondSuccess(c, fmt.Sprintf("找到 %d 条结果", len(results)), gin.H{
		"keyword":       keyword,
		"total_results": len(results),
		"results":       results,
	})
}

func (p *WeiboPlugin) buildUserTasks(users []*User) []UserTask {
	userOwners := make(map[string][]*User)

	for _, user := range users {
		for _, uid := range user.UserIDs {
			userOwners[uid] = append(userOwners[uid], user)
		}
	}

	tasks := []UserTask{}
	userTaskCount := make(map[string]int)

	for uid, owners := range userOwners {
		selectedUser := owners[0]
		minTasks := userTaskCount[selectedUser.Hash]

		for _, owner := range owners {
			if count := userTaskCount[owner.Hash]; count < minTasks {
				selectedUser = owner
				minTasks = count
			}
		}

		// 检查是否需要刷新Cookie（每小时刷新一次）
		cookie := selectedUser.Cookie
		if time.Since(selectedUser.LastRefresh) > time.Hour {
			if DebugLog {
				fmt.Printf("[Weibo] Cookie已使用超过1小时，刷新短期令牌...\n")
			}
			refreshedCookie := p.refreshCookie(cookie)
			if refreshedCookie != cookie {
				selectedUser.Cookie = refreshedCookie
				selectedUser.LastRefresh = time.Now()
				p.saveUser(selectedUser)
				cookie = refreshedCookie
				if DebugLog {
					fmt.Printf("[Weibo] Cookie刷新成功\n")
				}
			}
		}

		tasks = append(tasks, UserTask{
			UserID: uid,
			Cookie: cookie,
		})

		userTaskCount[selectedUser.Hash]++
	}

	return tasks
}

func (p *WeiboPlugin) refreshCookie(cookieStr string) string {
	// 访问PC端和移动端首页刷新短期令牌（XSRF-TOKEN等）
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	// 访问PC端首页
	reqPC, err := http.NewRequest("GET", "https://weibo.com/", nil)
	if err != nil {
		return cookieStr
	}
	reqPC.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	reqPC.Header.Set("Cookie", cookieStr)

	respPC, err := client.Do(reqPC)
	if err != nil {
		return cookieStr
	}
	respPC.Body.Close()

	// 访问移动端首页
	reqMobile, err := http.NewRequest("GET", "https://m.weibo.cn/", nil)
	if err != nil {
		return cookieStr
	}
	reqMobile.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 14_0 like Mac OS X) AppleWebKit/605.1.15")
	reqMobile.Header.Set("Cookie", cookieStr)

	respMobile, err := client.Do(reqMobile)
	if err != nil {
		return cookieStr
	}
	respMobile.Body.Close()

	// 合并响应中的新Cookie
	cookieMap := make(map[string]string)

	// 解析原始Cookie
	for _, item := range strings.Split(cookieStr, "; ") {
		if idx := strings.Index(item, "="); idx > 0 {
			key := item[:idx]
			value := item[idx+1:]
			cookieMap[key] = value
		}
	}

	// 更新PC端响应的Cookie
	for _, cookie := range respPC.Cookies() {
		if cookie.Value != "" {
			cookieMap[cookie.Name] = cookie.Value
		}
	}

	// 更新移动端响应的Cookie
	for _, cookie := range respMobile.Cookies() {
		if cookie.Value != "" {
			cookieMap[cookie.Name] = cookie.Value
		}
	}

	// 重新组合Cookie字符串
	var parts []string
	for k, v := range cookieMap {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}

	return strings.Join(parts, "; ")
}

func (p *WeiboPlugin) executeTasks(tasks []UserTask, keyword string) []model.SearchResult {
	// 发布截止：框架只等 AsyncResponseTimeout（默认 4 秒），到点还没交出的结果会被整体
	// 丢弃。单个用户要翻 3 页、每页还要拉一轮评论，实测要 8.5 秒，整批等待必然超线，
	// 所以到点先把已经拿到的结果交出去，未完成的请求在后台自行结束。
	publishDeadline := time.Now().Add(plugin.PublishBudget())

	var allResults []model.SearchResult
	skipped := 0
	var mu sync.Mutex
	var wg sync.WaitGroup

	semaphore := make(chan struct{}, MaxConcurrentWeibo)

	for _, task := range tasks {
		wg.Add(1)
		go func(t UserTask) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 预算已经用完，连请求都不发
			if time.Now().After(publishDeadline) {
				mu.Lock()
				skipped++
				mu.Unlock()
				return
			}

			results := p.searchUserWeibo(t.UserID, t.Cookie, keyword)

			mu.Lock()
			allResults = append(allResults, results...)
			mu.Unlock()
		}(task)
	}

	// 有界等待：到点就带着已经拿到的结果返回
	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()

	timedOut := false
	if remaining := time.Until(publishDeadline); remaining > 0 {
		select {
		case <-allDone:
		case <-time.After(remaining):
			timedOut = true
		}
	} else {
		timedOut = true
	}

	mu.Lock()
	snapshot := make([]model.SearchResult, len(allResults))
	copy(snapshot, allResults)
	skippedCount := skipped
	mu.Unlock()

	if timedOut {
		fmt.Printf("[Weibo] ⏱️ 触发发布截止，先返回 %d 条（未开始 %d 个 / 共 %d 个用户）；关键词 %q\n",
			len(snapshot), skippedCount, len(tasks), keyword)
	}

	return snapshot
}

func (p *WeiboPlugin) searchUserWeibo(uid, cookie, keyword string) []model.SearchResult {
	var results []model.SearchResult
	maxPages := 3

	// 单个请求超时收紧到 15 秒：发布窗口只有 4 秒，30 秒的客户端超时意味着
	// 请求即使已经被放弃，底层连接还要再挂着跑很久。
	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	for page := 1; page <= maxPages; page++ {
		apiURL := "https://weibo.com/ajax/profile/searchblog"

		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			if DebugLog {
				fmt.Printf("[Weibo] 创建请求失败: %v\n", err)
			}
			return results
		}

		q := req.URL.Query()
		q.Add("uid", uid)
		q.Add("feature", "0")
		q.Add("q", keyword)
		q.Add("page", fmt.Sprintf("%d", page))
		req.URL.RawQuery = q.Encode()

		req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req.Header.Set("Referer", "https://weibo.com/")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
		req.Header.Set("Cookie", cookie)

		if DebugLog {
			fmt.Printf("[Weibo] 请求URL: %s\n", req.URL.String())
			fmt.Printf("[Weibo] Cookie首100字符: %s\n", cookie[:min(100, len(cookie))])
		}

		resp, err := client.Do(req)
		if err != nil {
			if DebugLog {
				fmt.Printf("[Weibo] 请求失败: %v\n", err)
			}
			return results
		}

		body, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
		resp.Body.Close()
		if err != nil {
			if DebugLog {
				fmt.Printf("[Weibo] 读取响应失败: %v\n", err)
			}
			return results
		}

		if DebugLog {
			fmt.Printf("[Weibo] 响应状态码: %d\n", resp.StatusCode)
			if len(body) > 0 {
				fmt.Printf("[Weibo] 响应内容: %s\n", string(body)[:min(500, len(body))])
			}
		}

		if resp.StatusCode != 200 {
			if DebugLog {
				fmt.Printf("[Weibo] HTTP状态码错误: %d\n", resp.StatusCode)
			}
			return results
		}

		var apiResp map[string]interface{}
		if err := json.Unmarshal(body, &apiResp); err != nil {
			if DebugLog {
				fmt.Printf("[Weibo] JSON解析失败: %v, 原始内容: %s\n", err, string(body)[:min(200, len(body))])
			}
			return results
		}

		// ok字段判断：支持多种类型（json.Number, float64, int, bool等）
		okValue := apiResp["ok"]
		isOK := false

		if okValue != nil {
			okStr := fmt.Sprintf("%v", okValue)
			isOK = (okStr == "1" || okStr == "true")
		}

		if DebugLog {
			fmt.Printf("[Weibo] ok字段: %v (类型:%T), 判断结果: %v\n", okValue, okValue, isOK)
		}

		if !isOK {
			if DebugLog {
				fmt.Printf("[Weibo] API返回失败, msg=%v, 停止搜索\n", apiResp["msg"])
			}
			break
		}

		data, _ := apiResp["data"].(map[string]interface{})
		if data == nil {
			if DebugLog {
				fmt.Printf("[Weibo] data字段为nil\n")
			}
			break
		}

		list, _ := data["list"].([]interface{})

		if DebugLog {
			fmt.Printf("[Weibo] 第%d页返回%d条微博\n", page, len(list))
		}

		if len(list) == 0 {
			break
		}

		// 并发处理每条微博（获取评论）
		var wg sync.WaitGroup
		var mu sync.Mutex

		for i, item := range list {
			itemMap, _ := item.(map[string]interface{})
			wg.Add(1)
			go func(index int, weiboData map[string]interface{}) {
				defer wg.Done()

				result := p.parseWeibo(weiboData, uid)

				// 获取微博ID用于获取评论
				weiboID := ""
				if idStr, ok := weiboData["idstr"].(string); ok {
					weiboID = idStr
				} else if idNum, ok := weiboData["id"].(float64); ok {
					weiboID = fmt.Sprintf("%.0f", idNum)
				}

				if DebugLog {
					fmt.Printf("[Weibo] 微博%d: 标题=%s, 正文链接数=%d\n", index+1, result.Title[:min(30, len(result.Title))], len(result.Links))
				}

				// 如果正文没有网盘链接，才获取评论
				if len(result.Links) == 0 && weiboID != "" {
					if DebugLog {
						fmt.Printf("[Weibo] 正文无链接，获取评论...\n")
					}
					comments := p.getComments(weiboID, cookie, MaxComments)

					commentLinkCount := 0
					for _, comment := range comments {
						// 1. 从评论文本直接提取网盘链接
						commentLinks := extractNetworkDriveLinks(comment.Text, result.Datetime)

						// 2. 从评论中的URLs（已解码的sinaurl）提取网盘链接或抓取页面
						for _, decodedURL := range comment.URLs {
							// 先尝试直接匹配网盘链接
							directLinks := extractNetworkDriveLinks(decodedURL, result.Datetime)
							if len(directLinks) > 0 {
								commentLinks = append(commentLinks, directLinks...)
							} else {
								// 不是网盘链接，尝试抓取页面内容
								if DebugLog {
									fmt.Printf("[Weibo] 评论链接不是网盘，抓取页面: %s\n", decodedURL)
								}
								pageLinks := fetchPageAndExtractLinks(decodedURL, result.Datetime)
								commentLinks = append(commentLinks, pageLinks...)
							}
						}

						// 添加到结果
						result.Links = append(result.Links, commentLinks...)
						commentLinkCount += len(commentLinks)
					}

					if DebugLog {
						fmt.Printf("[Weibo] 获取%d条评论, 评论链接数=%d, 总链接数=%d\n", len(comments), commentLinkCount, len(result.Links))
					}
				}

				if len(result.Links) > 0 {
					mu.Lock()
					results = append(results, result)
					mu.Unlock()

					if DebugLog {
						fmt.Printf("[Weibo] ✓ 找到网盘链接: %s, 链接数: %d\n", result.Title, len(result.Links))
					}
				}
			}(i, itemMap)
		}

		wg.Wait()

		time.Sleep(time.Second)
	}

	if DebugLog {
		fmt.Printf("[Weibo] 用户%s搜索完成, 共%d条结果\n", uid, len(results))
	}
	return results
}

func (p *WeiboPlugin) getComments(weiboID, cookie string, maxComments int) []Comment {
	var comments []Comment
	maxID := 0
	maxIDType := 0

	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	for len(comments) < maxComments {
		apiURL := "https://m.weibo.cn/comments/hotflow"

		req, err := http.NewRequest("GET", apiURL, nil)
		if err != nil {
			if DebugLog {
				fmt.Printf("[Weibo] 创建评论请求失败: %v\n", err)
			}
			break
		}

		q := req.URL.Query()
		q.Add("id", weiboID)
		q.Add("mid", weiboID)
		q.Add("max_id", fmt.Sprintf("%d", maxID))
		q.Add("max_id_type", fmt.Sprintf("%d", maxIDType))
		req.URL.RawQuery = q.Encode()

		req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 14_0 like Mac OS X) AppleWebKit/605.1.15")
		req.Header.Set("Referer", "https://m.weibo.cn/")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("Cookie", cookie)

		if DebugLog {
			fmt.Printf("[Weibo] 获取评论: %s, max_id=%d\n", weiboID, maxID)
		}

		resp, err := client.Do(req)
		if err != nil {
			if DebugLog {
				fmt.Printf("[Weibo] 评论请求失败: %v\n", err)
			}
			break
		}

		body, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
		resp.Body.Close()
		if err != nil {
			if DebugLog {
				fmt.Printf("[Weibo] 读取评论响应失败: %v\n", err)
			}
			break
		}

		if resp.StatusCode != 200 {
			if DebugLog {
				fmt.Printf("[Weibo] 评论API状态码错误: %d\n", resp.StatusCode)
			}
			break
		}

		var apiResp map[string]interface{}
		if err := json.Unmarshal(body, &apiResp); err != nil {
			if DebugLog {
				fmt.Printf("[Weibo] 评论JSON解析失败: %v\n", err)
			}
			break
		}

		data, _ := apiResp["data"].(map[string]interface{})
		if data == nil {
			break
		}

		commentList, _ := data["data"].([]interface{})
		if len(commentList) == 0 {
			break
		}

		for _, item := range commentList {
			commentMap, _ := item.(map[string]interface{})
			rawText, _ := commentMap["text"].(string)

			cleanText := cleanHTML(rawText)
			urls := extractURLsFromComment(rawText)

			comments = append(comments, Comment{
				Text: cleanText,
				URLs: urls,
			})

			if len(comments) >= maxComments {
				break
			}
		}

		newMaxID := 0
		if maxIDVal, ok := data["max_id"].(float64); ok {
			newMaxID = int(maxIDVal)
		}

		if newMaxID == 0 || newMaxID == maxID {
			break
		}

		maxID = newMaxID
		if maxIDTypeVal, ok := data["max_id_type"].(float64); ok {
			maxIDType = int(maxIDTypeVal)
		}

		time.Sleep(500 * time.Millisecond)
	}

	if DebugLog && len(comments) > 0 {
		fmt.Printf("[Weibo] 获取到%d条评论\n", len(comments))
	}

	return comments
}

func extractURLsFromComment(htmlText string) []string {
	if htmlText == "" {
		return []string{}
	}

	pattern := weiboRe1
	matches := pattern.FindAllStringSubmatch(htmlText, -1)

	var urls []string
	for _, match := range matches {
		if len(match) > 1 {
			decoded, err := url.QueryUnescape(match[1])
			if err == nil {
				urls = append(urls, decoded)
			}
		}
	}

	return urls
}

// fetchPageAndExtractLinks 抓取页面内容并提取网盘链接
func fetchPageAndExtractLinks(pageURL string, datetime time.Time) []model.Link {
	client := &http.Client{
		Timeout: 15 * time.Second,
	}

	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return nil
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil
	}

	body, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		return nil
	}

	// 从HTML中提取网盘链接
	htmlContent := string(body)
	return extractNetworkDriveLinks(htmlContent, datetime)
}

type Comment struct {
	Text string
	URLs []string
}

func (p *WeiboPlugin) parseWeibo(weibo map[string]interface{}, uid string) model.SearchResult {
	// 优先使用text_raw，其次使用text
	textRaw, _ := weibo["text_raw"].(string)
	if textRaw == "" {
		textRaw, _ = weibo["text"].(string)
	}

	// 检查是否是长文本（需要额外请求获取完整内容）
	isLongText := false
	if longTextFlag, ok := weibo["isLongText"].(bool); ok && longTextFlag {
		isLongText = true
	}

	// 先获取发布时间
	createdAt, _ := weibo["created_at"].(string)
	publishTime := time.Now()
	if createdAt != "" {
		if t, err := time.Parse("Mon Jan 02 15:04:05 -0700 2006", createdAt); err == nil {
			publishTime = t
		}
	}

	text := cleanHTML(textRaw)

	if DebugLog && len(text) > 0 {
		truncated := ""
		if isLongText {
			truncated = " [长文本-可能被截断]"
		}
		fmt.Printf("[Weibo DEBUG] 微博原始文本%s: %s\n", truncated, text[:min(200, len(text))])
	}

	// 1. 直接从文本中提取网盘链接
	links := extractNetworkDriveLinks(text, publishTime)
	fmt.Println(links)

	// 2. 处理url_struct字段中的链接（包含所有外部链接，已由微博API解码）
	if urlStruct, ok := weibo["url_struct"].([]interface{}); ok && len(urlStruct) > 0 {
		if DebugLog {
			fmt.Printf("[Weibo DEBUG] 发现url_struct字段，包含%d个链接\n", len(urlStruct))
		}

		for _, urlItem := range urlStruct {
			if urlMap, ok := urlItem.(map[string]interface{}); ok {
				if urlMap["url_title"] != "网页链接" {
					continue
				}
				longURL, _ := urlMap["long_url"].(string)

				if longURL == "" {
					continue
				}

				if DebugLog {
					fmt.Printf("[Weibo DEBUG] url_struct中的长链接: %s\n", longURL)
				}

				// 先尝试直接匹配网盘链接
				directLinks := extractNetworkDriveLinks(longURL, publishTime)
				if len(directLinks) > 0 {
					links = append(links, directLinks...)
					if DebugLog {
						fmt.Printf("[Weibo DEBUG] url_struct直接匹配到网盘链接: %d个\n", len(directLinks))
					}
				} else {
					// 不是网盘链接，尝试抓取页面内容
					if DebugLog {
						fmt.Printf("[Weibo DEBUG] url_struct链接不是网盘，尝试抓取页面: %s\n", longURL)
					}
					pageLinks := fetchPageAndExtractLinks(longURL, publishTime)
					if len(pageLinks) > 0 {
						links = append(links, pageLinks...)
						if DebugLog {
							fmt.Printf("[Weibo DEBUG] 从url_struct页面提取到网盘链接: %d个\n", len(pageLinks))
						}
					}
				}
			}
		}
	}

	if DebugLog {
		fmt.Printf("[Weibo DEBUG] 最终共提取到%d个网盘链接\n", len(links))
	}

	title := text
	if len(text) > 100 {
		title = text[:100] + "..."
	}

	// 获取微博ID，支持多种类型
	id := ""
	if idStr, ok := weibo["idstr"].(string); ok {
		id = idStr
	} else if idStr, ok := weibo["id"].(string); ok {
		id = idStr
	} else if idNum, ok := weibo["id"].(float64); ok {
		id = fmt.Sprintf("%.0f", idNum)
	} else {
		// 如果以上都失败，尝试转字符串
		id = fmt.Sprintf("%v", weibo["id"])
	}

	return model.SearchResult{
		UniqueID: fmt.Sprintf("weibo-%s-%s", uid, id),
		Channel:  "",
		Datetime: publishTime,
		Title:    title,
		Content:  text,
		Links:    links,
	}
}

func extractNetworkDriveLinks(text string, datetime time.Time) []model.Link {
	var links []model.Link
	seenURLs := make(map[string]bool) // 用于去重

	patterns := map[string]string{
		"baidu":  `https?://pan\.baidu\.com/s/[a-zA-Z0-9_-]+(?:\?pwd=[a-zA-Z0-9]+)?`,
		"quark":  `https?://pan\.quark\.cn/s/[a-zA-Z0-9]+(?:\?pwd=[a-zA-Z0-9]+)?`,
		"aliyun": `https?://www\.alip?a?n\.com/s/[a-zA-Z0-9]+(?:\?[^\s]*)?|https?://www\.aliyundrive\.com/s/[a-zA-Z0-9]+(?:\?[^\s]*)?`,
		"115":    `https?://115\.com/s/[a-zA-Z0-9]+(?:\?[^\s]*)?`,
		"tianyi": `https?://cloud\.189\.cn/(?:t/|web/share\?code=)[a-zA-Z0-9]+(?:&?[^\s]*)?`,
		"xunlei": `https?://pan\.xunlei\.com/s/[a-zA-Z0-9_-]+(?:\?[^\s]*)?`,
		"123":    `https?://www\.123pan\.com/s/[a-zA-Z0-9_-]+(?:\?[^\s]*)?`,
		"pikpak": `https?://mypikpak\.com/s/[a-zA-Z0-9]+(?:\?[^\s]*)?`,
	}

	pwdPatterns := []string{
		`(?:密码|提取码|访问码|pwd|code)[:：\s]*([a-zA-Z0-9]{4})`,
		`pwd=([a-zA-Z0-9]{4})`,
	}

	for linkType, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindAllString(text, -1)

		for _, match := range matches {
			// 去重检查
			if seenURLs[match] {
				continue
			}
			seenURLs[match] = true

			password := ""
			start := strings.Index(text, match)
			if start != -1 {
				contextStart := start - 50
				if contextStart < 0 {
					contextStart = 0
				}
				contextEnd := start + len(match) + 50
				if contextEnd > len(text) {
					contextEnd = len(text)
				}
				context := text[contextStart:contextEnd]

				for _, pwdPattern := range pwdPatterns {
					pwdRe := regexp.MustCompile(pwdPattern)
					if pwdMatch := pwdRe.FindStringSubmatch(context); len(pwdMatch) > 1 {
						password = pwdMatch[1]
						break
					}
				}
			}

			links = append(links, model.Link{
				Type:     linkType,
				URL:      match,
				Password: password,
				Datetime: datetime,
			})
		}
	}

	return links
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func cleanHTML(html string) string {
	re := weiboRe2
	text := re.ReplaceAllString(html, "")
	text = strings.TrimSpace(text)
	text = weiboRe3.ReplaceAllString(text, " ")
	return text
}

type LoginResult struct {
	Status  string
	Cookie  string
	Message string
}

func (p *WeiboPlugin) checkQRLoginStatus(qrsig string) (*LoginResult, error) {
	// 参考Python auto.py第80行的check_qrcode_status实现
	// URL: https://passport.weibo.com/sso/v2/qrcode/check?entry=sso&qrid={qrid}&callback=STK_{timestamp}

	// 但我们使用qrsig而不是qrid，需要从session cookie中获取qrid
	// 实际上Python实现中，qrsig是从get_qrcode的响应中提取的qrid
	// 这里我们用qrsig作为qrid

	timestamp := time.Now().UnixMilli()
	checkURL := fmt.Sprintf("https://passport.weibo.com/sso/v2/qrcode/check?entry=sso&qrid=%s&callback=STK_%d", qrsig, timestamp)

	fmt.Printf("[Weibo DEBUG] checkQRLoginStatus调用 - qrsig: %s\n", qrsig)
	fmt.Printf("[Weibo DEBUG] checkURL: %s\n", checkURL)

	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequest("GET", checkURL, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/86.0.4240.111 Safari/537.36")
	req.Header.Set("Referer", "https://weibo.com/")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := util.ReadAllLimited(resp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		return nil, err
	}

	// 响应可能是JSONP格式: STK_xxx({...}) 或纯JSON格式: {...}
	responseText := string(body)

	fmt.Printf("[Weibo DEBUG] 原始响应: %s\n", responseText)

	// 提取JSON部分
	var jsonStr string
	if strings.HasPrefix(responseText, "STK_") {
		// JSONP格式: STK_xxx({...})
		startIdx := strings.Index(responseText, "({")
		endIdx := strings.LastIndex(responseText, "})")
		if startIdx == -1 || endIdx == -1 {
			fmt.Printf("[Weibo DEBUG] JSONP格式解析失败\n")
			return &LoginResult{Status: "waiting"}, nil
		}
		jsonStr = responseText[startIdx+1 : endIdx+1]
	} else if strings.HasPrefix(responseText, "{") {
		// 纯JSON格式: {...}
		jsonStr = responseText
		fmt.Printf("[Weibo DEBUG] 检测到纯JSON格式响应\n")
	} else {
		fmt.Printf("[Weibo DEBUG] 未知响应格式\n")
		return &LoginResult{Status: "waiting"}, nil
	}

	var result struct {
		Retcode int    `json:"retcode"`
		Msg     string `json:"msg"`
		Data    struct {
			URL string `json:"url"`
		} `json:"data"`
	}

	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		fmt.Printf("[Weibo DEBUG] JSON解析失败: %v, JSON字符串: %s\n", err, jsonStr)
		return &LoginResult{Status: "waiting"}, nil
	}

	fmt.Printf("[Weibo DEBUG] 解析后retcode: %d, msg: %s\n", result.Retcode, result.Msg)

	// 参考Python auto.py第93-108行的状态码处理
	// 20000000: 扫码成功
	// 50114001: 等待扫码
	// 50114002: 已扫描，等待确认
	// 50114004: 二维码已过期

	if result.Retcode == 20000000 {
		// 登录成功，需要初始化Cookie
		alt := result.Data.URL
		fmt.Printf("[Weibo DEBUG] 登录成功! alt URL: %s\n", alt)

		cookieStr, err := p.initCookieFromAlt(alt)
		if err != nil {
			fmt.Printf("[Weibo DEBUG] 初始化Cookie失败: %v\n", err)
			return nil, fmt.Errorf("初始化Cookie失败: %v", err)
		}

		fmt.Printf("[Weibo DEBUG] Cookie初始化成功, Cookie长度: %d\n", len(cookieStr))
		return &LoginResult{Status: "success", Cookie: cookieStr}, nil
	} else if result.Retcode == 50114002 {
		// 已扫描，等待确认
		fmt.Printf("[Weibo DEBUG] 已扫描，等待确认\n")
		return &LoginResult{Status: "waiting", Message: "已扫描，请在手机上确认"}, nil
	} else if result.Retcode == 50114004 {
		// 二维码已过期
		fmt.Printf("[Weibo DEBUG] 二维码已过期\n")
		return &LoginResult{Status: "expired", Message: "二维码已过期"}, nil
	}

	// 默认状态：等待扫码
	fmt.Printf("[Weibo DEBUG] 等待扫码中, retcode: %d\n", result.Retcode)
	return &LoginResult{Status: "waiting", Message: "等待扫码中"}, nil
}

func (p *WeiboPlugin) generateQRCodeWithSig() ([]byte, string, error) {
	// 参考Python auto.py第46-75行的get_qrcode实现
	// 第一步：获取二维码信息（包含api_key和qrid）
	timestamp := time.Now().UnixMilli()
	infoURL := fmt.Sprintf("https://passport.weibo.com/sso/v2/qrcode/image?entry=miniblog&size=180&callback=STK_%d", timestamp)

	client := &http.Client{Timeout: 15 * time.Second}

	req, err := http.NewRequest("GET", infoURL, nil)
	if err != nil {
		return nil, "", err
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/86.0.4240.111 Safari/537.36")
	req.Header.Set("Referer", "https://weibo.com/")

	infoResp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer infoResp.Body.Close()

	infoBody, err := util.ReadAllLimited(infoResp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		return nil, "", err
	}

	// 响应是JSONP格式，提取JSON部分
	infoText := string(infoBody)

	// 提取api_key: 正则 api_key=(.*)"
	apiKeyRegex := weiboRe4
	apiKeyMatch := apiKeyRegex.FindStringSubmatch(infoText)
	if len(apiKeyMatch) < 2 {
		return nil, "", fmt.Errorf("无法提取api_key")
	}
	apiKey := apiKeyMatch[1]

	// 提取qrid: 正则 "qrid":"(.*?)"
	qridRegex := weiboRe5
	qridMatch := qridRegex.FindStringSubmatch(infoText)
	if len(qridMatch) < 2 {
		return nil, "", fmt.Errorf("无法提取qrid")
	}
	qrid := qridMatch[1]

	// 第二步：使用api_key获取二维码图片
	qrImageURL := fmt.Sprintf("https://v2.qr.weibo.cn/inf/gen?api_key=%s", apiKey)

	qrReq, err := http.NewRequest("GET", qrImageURL, nil)
	if err != nil {
		return nil, "", err
	}

	qrReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/86.0.4240.111 Safari/537.36")
	qrReq.Header.Set("Referer", "https://weibo.com/")

	qrResp, err := client.Do(qrReq)
	if err != nil {
		return nil, "", err
	}
	defer qrResp.Body.Close()

	qrcodeBytes, err := util.ReadAllLimited(qrResp.Body, util.MaxUpstreamResponseBytes)
	if err != nil {
		return nil, "", err
	}

	// 返回二维码图片和qrid（用于后续的登录状态检查）
	return qrcodeBytes, qrid, nil
}

func (p *WeiboPlugin) initCookieFromAlt(alt string) (string, error) {
	// 参考Python auto.py第118-146行的init_cookie实现
	// 访问alt URL获取PC端Cookie，然后访问移动端获取移动端Cookie

	fmt.Printf("[Weibo DEBUG] initCookieFromAlt开始 - alt URL: %s\n", alt)

	jar, err := cookiejar.New(nil)
	if err != nil {
		fmt.Printf("[Weibo DEBUG] 创建cookiejar失败: %v\n", err)
		return "", err
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 允许重定向，但保留Cookie
			return nil
		},
	}

	// 第一步：访问alt URL（允许重定向）
	req1, err := http.NewRequest("GET", alt, nil)
	if err != nil {
		return "", err
	}
	req1.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/86.0.4240.111 Safari/537.36")
	req1.Header.Set("Referer", "https://weibo.com/")

	resp1, err := client.Do(req1)
	if err != nil {
		fmt.Printf("[Weibo DEBUG] 访问alt URL失败: %v\n", err)
		return "", err
	}
	resp1.Body.Close()
	fmt.Printf("[Weibo DEBUG] 步骤1完成: 访问alt URL, 状态码: %d\n", resp1.StatusCode)

	// 第二步：访问weibo.com首页
	req2, err := http.NewRequest("GET", "https://weibo.com/", nil)
	if err != nil {
		return "", err
	}
	req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/86.0.4240.111 Safari/537.36")
	req2.Header.Set("Referer", "https://weibo.com/")

	resp2, err := client.Do(req2)
	if err != nil {
		fmt.Printf("[Weibo DEBUG] 访问weibo.com失败: %v\n", err)
		return "", err
	}
	resp2.Body.Close()
	fmt.Printf("[Weibo DEBUG] 步骤2完成: 访问weibo.com, 状态码: %d\n", resp2.StatusCode)

	// 第三步：访问移动端首页
	req3, err := http.NewRequest("GET", "https://m.weibo.cn/", nil)
	if err != nil {
		return "", err
	}
	req3.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 14_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148")
	req3.Header.Set("Referer", "https://m.weibo.cn/")

	resp3, err := client.Do(req3)
	if err != nil {
		fmt.Printf("[Weibo DEBUG] 访问m.weibo.cn失败: %v\n", err)
		return "", err
	}
	resp3.Body.Close()
	fmt.Printf("[Weibo DEBUG] 步骤3完成: 访问m.weibo.cn, 状态码: %d\n", resp3.StatusCode)

	// 第四步：访问移动端profile页面
	req4, err := http.NewRequest("GET", "https://m.weibo.cn/profile", nil)
	if err != nil {
		return "", err
	}
	req4.Header.Set("User-Agent", "Mozilla/5.0 (iPhone; CPU iPhone OS 14_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148")
	req4.Header.Set("Referer", "https://m.weibo.cn/")

	resp4, err := client.Do(req4)
	if err != nil {
		fmt.Printf("[Weibo DEBUG] 访问m.weibo.cn/profile失败: %v\n", err)
		return "", err
	}
	resp4.Body.Close()
	fmt.Printf("[Weibo DEBUG] 步骤4完成: 访问m.weibo.cn/profile, 状态码: %d\n", resp4.StatusCode)

	// 收集所有Cookie
	allCookies := make(map[string]string)

	// 从cookie jar中提取所有Cookie
	weiboURL, _ := url.Parse("https://weibo.com")
	weiboCNURL, _ := url.Parse("https://m.weibo.cn")

	for _, cookie := range jar.Cookies(weiboURL) {
		allCookies[cookie.Name] = cookie.Value
	}
	for _, cookie := range jar.Cookies(weiboCNURL) {
		allCookies[cookie.Name] = cookie.Value
	}

	fmt.Printf("[Weibo DEBUG] 收集到的Cookie字段: %v\n", func() []string {
		keys := make([]string, 0, len(allCookies))
		for k := range allCookies {
			keys = append(keys, k)
		}
		return keys
	}())

	// 检查必需的Cookie字段
	requiredFields := []string{"SUB", "SUBP"}
	for _, field := range requiredFields {
		if _, exists := allCookies[field]; !exists {
			fmt.Printf("[Weibo DEBUG] 缺少必需的Cookie字段: %s\n", field)
			return "", fmt.Errorf("缺少必需的Cookie字段: %s", field)
		} else {
			fmt.Printf("[Weibo DEBUG] ✓ 找到必需字段: %s\n", field)
		}
	}

	// 构建Cookie字符串
	cookieParts := make([]string, 0, len(allCookies))
	for k, v := range allCookies {
		cookieParts = append(cookieParts, fmt.Sprintf("%s=%s", k, v))
	}

	cookieStr := strings.Join(cookieParts, "; ")
	fmt.Printf("[Weibo DEBUG] Cookie初始化完成, 总长度: %d, 字段数: %d\n", len(cookieStr), len(allCookies))

	return cookieStr, nil
}

func (p *WeiboPlugin) generateHash(input string) string {
	salt := os.Getenv("WEIBO_HASH_SALT")
	if salt == "" {
		salt = "pansou_weibo_secret_2025"
	}
	data := input + salt
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

func (p *WeiboPlugin) isHexString(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func respondSuccess(c *gin.Context, message string, data interface{}) {
	c.JSON(200, gin.H{
		"success": true,
		"message": message,
		"data":    data,
	})
}

func respondError(c *gin.Context, message string) {
	c.JSON(200, gin.H{
		"success": false,
		"message": message,
		"data":    nil,
	})
}

func (p *WeiboPlugin) startCleanupTask() {
	ticker := time.NewTicker(24 * time.Hour)
	for range ticker.C {
		deleted := p.cleanupExpiredUsers()
		marked := p.markInactiveUsers()

		if deleted > 0 || marked > 0 {
			fmt.Printf("[Weibo] 清理任务完成: 删除 %d 个过期用户, 标记 %d 个不活跃用户\n", deleted, marked)
		}
	}
}

func (p *WeiboPlugin) cleanupExpiredUsers() int {
	deletedCount := 0
	now := time.Now()
	expireThreshold := now.AddDate(0, 0, -30)

	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)
		if user.Status == "expired" && user.LastAccessAt.Before(expireThreshold) {
			if err := p.deleteUser(user.Hash); err == nil {
				deletedCount++
			}
		}
		return true
	})

	return deletedCount
}

func (p *WeiboPlugin) markInactiveUsers() int {
	markedCount := 0
	now := time.Now()
	inactiveThreshold := now.AddDate(0, 0, -90)

	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)
		if user.LastAccessAt.Before(inactiveThreshold) && user.Status != "expired" {
			user.Status = "expired"
			user.Cookie = ""

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
	weiboRe1 = regexp.MustCompile(`https://weibo\.cn/sinaurl\?u=([^"&\s]+)`)
	weiboRe2 = regexp.MustCompile(`<[^>]+>`)
	weiboRe3 = regexp.MustCompile(`\s+`)
	weiboRe4 = regexp.MustCompile(`api_key=([^"]+)`)
	weiboRe5 = regexp.MustCompile(`"qrid":"([^"]+)"`)
)
