package gying

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"pansou/config"
	"pansou/model"
	"pansou/plugin"
	"pansou/util/json"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/idna"
	"golang.org/x/net/proxy"

	cloudscraper "github.com/Advik-B/cloudscraper/lib"
)

// 插件配置参数
const (
	MaxConcurrentUsers   = 10    // 最多使用的用户数
	MaxConcurrentDetails = 50    // 最大并发详情请求数
	DebugLog             = false // 调试日志开关（排查问题时改为true）
)

// 默认账户配置（可通过Web界面添加更多账户）
// 用户数据会保存到文件，重启后自动恢复
const (
	DefaultGyingBaseURL = "https://www.xn--wcv59z.com"
	GyingConfigFileName = "gying_config.json"
)

var legacyGyingHosts = map[string]struct{}{
	"gying.net":      {},
	"www.gying.net":  {},
	"xn--wcv59z.com": {},
}

var (
	challengeJSONPattern  = regexp.MustCompile(`(?s)const\s+json\s*=\s*(\{.*?\})\s*;\s*const\s+jss\s*=`)
	searchDataPattern     = regexp.MustCompile(`(?s)_obj\s*\.\s*search\s*=\s*(\{.*?\})\s*;`)
	accessCodeBlockRegex  = regexp.MustCompile(`[（(]\s*访问码[:：]\s*[^)）]+[)）]`)
	yearSuffixRegex       = regexp.MustCompile(`[（(]\d{4}[)）]`)
	baiduLinkRegex        = regexp.MustCompile(`https?://pan\.baidu\.com/s/[a-zA-Z0-9_-]+(?:\?pwd=[a-zA-Z0-9]{4})?`)
	quarkLinkRegex        = regexp.MustCompile(`https?://pan\.quark\.cn/s/[a-zA-Z0-9]+`)
	aliyunLinkRegex       = regexp.MustCompile(`https?://(?:www\.)?(?:alipan|aliyundrive)\.com/s/[a-zA-Z0-9]+`)
	xunleiLinkRegex       = regexp.MustCompile(`https?://pan\.xunlei\.com/s/[a-zA-Z0-9]+(?:\?pwd=[a-zA-Z0-9]{4})?`)
	tianyiLinkRegex       = regexp.MustCompile(`https?://cloud\.189\.cn/(?:t/|web/share\?code=)[a-zA-Z0-9]+`)
	tianyiShareCodeRegex  = regexp.MustCompile(`(?i)sharecode=([a-zA-Z0-9]+)`)
	tianyiCloudRegex      = regexp.MustCompile(`https?://(?:www\.)?tianyi\.cloud/[^\s<>"']+`)
	ucLinkRegex           = regexp.MustCompile(`https?://drive\.uc\.cn/s/[a-zA-Z0-9]+(?:\?public=\d+)?`)
	link123Regex          = regexp.MustCompile(`https?://(?:www\.)?123(?:684|685|865|912|pan|592)\.(?:com|cn)/s/[a-zA-Z0-9_-]+(?:\?pwd=[a-zA-Z0-9]{4,8})?`)
	link115Regex          = regexp.MustCompile(`https?://(?:115\.com|115cdn\.com|anxia\.com)/s/[a-zA-Z0-9]+(?:\?password=[a-zA-Z0-9]{4,8})?`)
	mobileYunLinkRegex    = regexp.MustCompile(`https?://yun\.139\.com/shareweb/#/w/i/[a-zA-Z0-9]+`)
	mobileCaiyunLinkRegex = regexp.MustCompile(`https?://(?:www\.)?caiyun\.139\.com/(?:w/i/[a-zA-Z0-9]+|m/i\?[a-zA-Z0-9]+)[^\s<>"']*`)
	mobileFeixinLinkRegex = regexp.MustCompile(`https?://caiyun\.feixin\.10086\.cn/[a-zA-Z0-9]+`)
	exactPasswordRegex    = regexp.MustCompile(`(?i)^[a-zA-Z0-9]{4,8}$`)
	magnetHashRegex       = regexp.MustCompile(`(?i)^[a-f0-9]{40}$`)
)

var inlinePasswordPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)[?&]pwd=([a-zA-Z0-9]{4,8})`),
	regexp.MustCompile(`(?i)[?&]password=([a-zA-Z0-9]{4,8})`),
	regexp.MustCompile(`访问码[:：]\s*([a-zA-Z0-9]{4,8})`),
	regexp.MustCompile(`提取码[:：]\s*([a-zA-Z0-9]{4,8})`),
	regexp.MustCompile(`密码[:：]\s*([a-zA-Z0-9]{4,8})`),
}

var gyingPanTypeMap = map[int]string{
	0: "xunlei",
	1: "baidu",
	2: "quark",
	3: "tianyi",
	4: "mobile",
	5: "115",
	6: "123",
	7: "uc",
	8: "aliyun",
}

var DefaultAccounts = []struct {
	Username string
	Password string
}{
	// 请使用 Web 接口添加用户：
	// POST /gying/add_user?username=xxx&password=xxx
}

// 存储目录
var StorageDir string

// 初始化存储目录

// HTML模板
const HTMLTemplate = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>PanSou Gying搜索配置</title>
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
        .form-group {
            margin-bottom: 15px;
        }
        .form-group label {
            display: block;
            margin-bottom: 5px;
            font-weight: bold;
        }
        .form-group input {
            width: 100%;
            padding: 10px;
            border: 1px solid #ddd;
            border-radius: 6px;
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
        .notice {
            background: #fff7d6;
            color: #744210;
            padding: 12px 15px;
            border-radius: 6px;
            margin-bottom: 15px;
            border: 1px solid #f6e05e;
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
    </style>
</head>
<body>
    <div class="container">
        <div class="header">
            <h1>🔍 PanSou Gying搜索</h1>
            <p>配置你的专属搜索服务</p>
            <p style="font-size: 12px; margin-top: 10px; opacity: 0.8;">
                🔗 当前地址: <span id="current-url">HASH_PLACEHOLDER</span>
            </p>
        </div>

        <div class="section" id="site-section">
            <div class="section-title">🌐 站点地址</div>

            <div class="status-box">
                <div class="status-item">
                    <span>当前站点</span>
                    <span id="base-url-current">-</span>
                </div>
            </div>

            <div class="form-group">
                <label>站点地址</label>
                <input type="text" id="base-url" placeholder="例如: https://www.教父.com">
            </div>
            <button class="btn btn-primary" onclick="saveBaseURL()">保存站点地址</button>
            <p style="margin-top: 10px; font-size: 12px; color: #666;">
                修改站点地址后，会清空当前登录状态，需要重新登录账号。
            </p>
        </div>

        <div class="section" id="login-section">
            <div class="section-title">🔐 登录状态</div>

            <div class="notice">登录前请先确认上方站点地址是否正确。</div>
            
            <div id="logged-in-view" class="hidden">
                <div class="status-box">
                    <div class="status-item">
                        <span>状态</span>
                        <span><strong style="color: #48bb78;">✅ 已登录</strong></span>
                    </div>
                    <div class="status-item">
                        <span>用户名</span>
                        <span id="username-display">-</span>
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
                <div id="alert-box"></div>
                <div class="form-group">
                    <label>用户名</label>
                    <input type="text" id="username" placeholder="输入用户名">
                </div>
                <div class="form-group">
                    <label>密码</label>
                    <input type="password" id="password" placeholder="输入密码">
                </div>
                <button class="btn btn-primary" onclick="login()">登录</button>
            </div>
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
            
            <p style="margin-bottom: 15px;">你可以通过API程序化管理：</p>

            <details>
                <summary style="cursor: pointer; padding: 10px 0; font-weight: bold;">登录</summary>
                <div style="background: #2d3748; color: #68d391; padding: 10px; border-radius: 6px; font-family: monospace; font-size: 12px; overflow-x: auto;">curl -X POST https://your-domain.com/gying/HASH_PLACEHOLDER \
  -H "Content-Type: application/json" \
  -d '{"action": "login", "username": "user", "password": "pass"}'</div>
            </details>
        </div>
    </div>

    <script>
        const HASH = 'HASH_PLACEHOLDER';
        const API_URL = '/gying/' + HASH;
        let statusCheckInterval = null;

        window.onload = function() {
            updateStatus();
            loadConfig();
            startStatusPolling();
        };

        function startStatusPolling() {
            statusCheckInterval = setInterval(updateStatus, 5000);
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
                    
                    document.getElementById('username-display').textContent = data.username || '-';
                    document.getElementById('login-time').textContent = data.login_time || '-';
                    document.getElementById('expire-info').textContent = '剩余 ' + (data.expires_in_days || 0) + ' 天';
                } else {
                    document.getElementById('logged-in-view').classList.add('hidden');
                    document.getElementById('not-logged-in-view').classList.remove('hidden');
                }
            }
        }

        async function loadConfig() {
            const result = await postAction('get_config');
            if (result.success && result.data) {
                const baseURL = result.data.base_url || '';
                document.getElementById('base-url').value = baseURL;
                document.getElementById('base-url-current').textContent = baseURL || '-';
            }
        }

        function showAlert(message, type = 'success') {
            const alertBox = document.getElementById('alert-box');
            alertBox.innerHTML = '<div class="alert alert-' + type + '">' + message + '</div>';
            setTimeout(() => {
                alertBox.innerHTML = '';
            }, 3000);
        }

        async function login() {
            const username = document.getElementById('username').value.trim();
            const password = document.getElementById('password').value.trim();
            
            if (!username || !password) {
                showAlert('请输入用户名和密码', 'error');
                return;
            }

            const result = await postAction('login', { username, password });
            if (result.success) {
                showAlert(result.message);
                updateStatus();
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

        async function saveBaseURL() {
            const baseURL = document.getElementById('base-url').value.trim();

            if (!baseURL) {
                showAlert('请输入站点地址', 'error');
                return;
            }

            const result = await postAction('update_config', { base_url: baseURL });
            if (result.success) {
                showAlert(result.message);
                loadConfig();
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
                        html += '[' + link.type + '] ';
                        if (link.work_title) {
                            html += '<strong>' + link.work_title + '</strong><br>';
                        }
                        html += link.url;
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

// GyingPlugin 插件结构
type GyingPlugin struct {
	*plugin.BaseAsyncPlugin
	users       sync.Map // 内存缓存：hash -> *User
	scrapers    sync.Map // cloudscraper实例缓存：hash -> *cloudscraper.Scraper
	mu          sync.RWMutex
	searchCache sync.Map // 插件级缓存：关键词->model.PluginSearchResult
	baseURL     string   // 当前配置的站点地址
	initialized bool     // 初始化状态标记
}

// User 用户数据结构
type User struct {
	Hash              string    `json:"hash"`
	Username          string    `json:"username"`           // 用户名
	EncryptedPassword string    `json:"encrypted_password"` // 加密后的密码（用于重启恢复）
	Cookie            string    `json:"cookie"`             // 登录Cookie字符串（仅供参考）
	Status            string    `json:"status"`             // pending/active/expired
	CreatedAt         time.Time `json:"created_at"`
	LoginAt           time.Time `json:"login_at"`
	ExpireAt          time.Time `json:"expire_at"`
	LastAccessAt      time.Time `json:"last_access_at"`
}

// SearchData 搜索页面JSON数据结构
type SearchData struct {
	Q  string   `json:"q"`  // 搜索关键词
	WD []string `json:"wd"` // 分词
	N  string   `json:"n"`  // 结果数量
	L  struct {
		Title  []string `json:"title"`  // 标题数组
		Year   []int    `json:"year"`   // 年份数组
		D      []string `json:"d"`      // 类型数组（mv/ac/tv）
		I      []string `json:"i"`      // 资源ID数组
		Info   []string `json:"info"`   // 信息数组
		Daoyan []string `json:"daoyan"` // 导演数组
		Zhuyan []string `json:"zhuyan"` // 主演数组
	} `json:"l"`
}

// DetailData 详情接口JSON数据结构
type DetailData struct {
	Code     int  `json:"code"`
	WP       bool `json:"wp"`
	Downlist struct {
		IMDB string `json:"imdb"`
		Type struct {
			A []string `json:"a"`
			B []string `json:"b"`
		} `json:"type"`
		Hex  string `json:"hex"`
		List struct {
			M []string      `json:"m"` // 磁力hash
			T []string      `json:"t"` // 资源名称
			S []string      `json:"s"` // 文件大小
			E []interface{} `json:"e"`
			P []string      `json:"p"` // 资源分组标识
			U []string      `json:"u"`
			K []interface{} `json:"k"`
			N []string      `json:"n"` // 更新时间
		} `json:"list"`
	} `json:"downlist"`
	Panlist struct {
		ID    []string `json:"id"`
		Name  []string `json:"name"`
		P     []string `json:"p"`     // 提取码数组
		URL   []string `json:"url"`   // 链接数组
		Type  []int    `json:"type"`  // 类型标识
		User  []string `json:"user"`  // 分享用户
		Time  []string `json:"time"`  // 分享时间
		TName []string `json:"tname"` // 网盘类型名称
	} `json:"panlist"`
}

type ChallengePageData struct {
	ID        string   `json:"id"`
	Challenge []string `json:"challenge"`
	Diff      int      `json:"diff"`
	Salt      string   `json:"salt"`
	N         string   `json:"N"`
	X         string   `json:"x"`
	T         int      `json:"t"`
}

type challengeVerifyResponse struct {
	Success bool   `json:"success"`
	Msg     string `json:"msg"`
}

type GyingConfig struct {
	BaseURL   string    `json:"base_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

func init() {
	p := &GyingPlugin{
		BaseAsyncPlugin: plugin.NewBaseAsyncPlugin("gying", 3),
	}

	plugin.RegisterGlobalPlugin(p)
}

func normalizeBaseURL(raw string) (string, error) {
	baseURL := strings.TrimSpace(raw)
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return "", fmt.Errorf("站点地址不能为空")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "https://" + baseURL
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("站点地址格式错误: %v", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("站点地址必须以 http:// 或 https:// 开头")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("站点地址缺少域名")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("站点地址不能包含参数或锚点")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", fmt.Errorf("站点地址不能包含路径")
	}

	// HTTP clients do not consistently convert Unicode hostnames to IDNA.
	// Store the ASCII form so both the config page and requests are reliable.
	hostname, err := idna.Lookup.ToASCII(parsed.Hostname())
	if err != nil {
		return "", fmt.Errorf("站点域名格式错误: %v", err)
	}
	host := hostname
	if port := parsed.Port(); port != "" {
		host += ":" + port
	}
	return parsed.Scheme + "://" + host, nil
}

func isLegacyGyingBaseURL(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	_, ok := legacyGyingHosts[strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))]
	return ok
}

func (p *GyingPlugin) configPath() string {
	return filepath.Join(StorageDir, GyingConfigFileName)
}

func (p *GyingPlugin) getBaseURL() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.baseURL == "" {
		return DefaultGyingBaseURL
	}
	return p.baseURL
}

func (p *GyingPlugin) setBaseURL(baseURL string) {
	p.mu.Lock()
	p.baseURL = baseURL
	p.mu.Unlock()
}

func (p *GyingPlugin) getLoginPageURL() string {
	return p.getBaseURL()
}

func (p *GyingPlugin) getLoginAPIURL() string {
	return p.getBaseURL() + "/user/login"
}

func (p *GyingPlugin) getWarmupDetailURL() string {
	return p.getBaseURL() + "/mv/wkMn"
}

func (p *GyingPlugin) loadConfig() error {
	p.setBaseURL(DefaultGyingBaseURL)

	filePath := p.configPath()
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var config GyingConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return err
	}
	if config.BaseURL == "" {
		return nil
	}

	baseURL, err := normalizeBaseURL(config.BaseURL)
	if err != nil {
		return err
	}
	if isLegacyGyingBaseURL(baseURL) {
		baseURL = DefaultGyingBaseURL
		if err := p.saveConfig(baseURL); err != nil {
			return fmt.Errorf("迁移旧站点配置失败: %w", err)
		}
	}
	p.setBaseURL(baseURL)
	return nil
}

func (p *GyingPlugin) saveConfig(baseURL string) error {
	config := GyingConfig{
		BaseURL:   baseURL,
		UpdatedAt: time.Now(),
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(p.configPath(), data, 0644)
}

func (p *GyingPlugin) clearSearchCache() {
	p.searchCache.Range(func(key, value interface{}) bool {
		p.searchCache.Delete(key)
		return true
	})
}

func (p *GyingPlugin) resetSessionsForBaseURLChange() {
	p.scrapers.Range(func(key, value interface{}) bool {
		p.scrapers.Delete(key)
		return true
	})

	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)
		user.Cookie = ""
		user.Status = "pending"
		user.LastAccessAt = time.Now()
		if err := p.saveUser(user); err != nil && DebugLog {
			fmt.Printf("[Gying] 切换站点后保存用户状态失败: %v\n", err)
		}
		return true
	})

	p.clearSearchCache()
}

func (p *GyingPlugin) updateBaseURL(rawBaseURL string) (string, error) {
	baseURL, err := normalizeBaseURL(rawBaseURL)
	if err != nil {
		return "", err
	}

	oldBaseURL := p.getBaseURL()
	p.setBaseURL(baseURL)
	if err := p.saveConfig(baseURL); err != nil {
		p.setBaseURL(oldBaseURL)
		return "", err
	}

	if baseURL != oldBaseURL {
		p.resetSessionsForBaseURLChange()
	}

	return baseURL, nil
}

// Initialize 实现 InitializablePlugin 接口，延迟初始化插件
func (p *GyingPlugin) Initialize() error {
	if p.initialized {
		return nil
	}

	// 初始化存储目录路径
	cachePath := os.Getenv("CACHE_PATH")
	if cachePath == "" {
		cachePath = "./cache"
	}
	StorageDir = filepath.Join(cachePath, "gying_users")

	// 初始化存储目录
	if err := os.MkdirAll(StorageDir, 0755); err != nil {
		return fmt.Errorf("创建存储目录失败: %v", err)
	}

	// 加载站点配置
	if err := p.loadConfig(); err != nil {
		return fmt.Errorf("加载站点配置失败: %v", err)
	}

	// 加载所有用户到内存
	p.loadAllUsers()

	// 异步初始化默认账户（不阻塞启动）
	go func() {
		// 延迟1秒，等待主程序完全启动
		time.Sleep(1 * time.Second)
		p.initDefaultAccounts()
	}()

	// 启动定期清理任务
	go p.startCleanupTask()

	// 启动session保活任务（防止session超时）
	go p.startSessionKeepAlive()

	p.initialized = true
	return nil
}

// ============ 插件接口实现 ============

// RegisterWebRoutes 注册Web路由
func (p *GyingPlugin) RegisterWebRoutes(router *gin.RouterGroup) {
	gying := router.Group("/gying")
	gying.GET("/:param", p.handleManagePage)
	gying.POST("/:param", p.handleManagePagePOST)

	fmt.Printf("[Gying] Web路由已注册: /gying/:param\n")
}

// Search 执行搜索并返回结果
func (p *GyingPlugin) Search(keyword string, ext map[string]interface{}) ([]model.SearchResult, error) {
	result, err := p.SearchWithResult(keyword, ext)
	if err != nil {
		return nil, err
	}
	return result.Results, nil
}

// SearchWithResult 执行搜索并返回包含IsFinal标记的结果
// 注意：gying插件不使用AsyncSearchWithResult的缓存机制，因为：
// 1. 使用自己的cloudscraper实例而不是传入的http.Client
// 2. 有自己的用户会话管理
// 3. Service层已经有缓存，无需插件层再次缓存
func (p *GyingPlugin) SearchWithResult(keyword string, ext map[string]interface{}) (model.PluginSearchResult, error) {
	// 解析 ext["refresh"]
	forceRefresh := false
	if ext != nil {
		if v, ok := ext["refresh"]; ok {
			if b, ok := v.(bool); ok && b {
				forceRefresh = true
			}
		}
	}

	if !forceRefresh {
		if cacheItem, ok := p.searchCache.Load(keyword); ok {
			cached := cacheItem.(model.PluginSearchResult)
			if DebugLog {
				fmt.Printf("[Gying] 命中插件缓存: %s\n", keyword)
			}
			return cached, nil
		}
	} else {
		if DebugLog {
			fmt.Printf("[Gying] 强制刷新，此次跳过插件缓存，关键词: %s\n", keyword)
		}
	}

	// 原有真实抓取逻辑
	if DebugLog {
		fmt.Printf("[Gying] searchWithScraper REAL 执行: %s\n", keyword)
	}
	users := p.getActiveUsers()
	if DebugLog {
		fmt.Printf("[Gying] 找到 %d 个有效用户\n", len(users))
	}
	if len(users) == 0 {
		if DebugLog {
			fmt.Printf("[Gying] 没有有效用户，返回空结果\n")
		}
		return model.PluginSearchResult{Results: []model.SearchResult{}, IsFinal: true}, nil
	}
	if len(users) > MaxConcurrentUsers {
		sort.Slice(users, func(i, j int) bool {
			return users[i].LastAccessAt.After(users[j].LastAccessAt)
		})
		users = users[:MaxConcurrentUsers]
	}
	results := p.executeSearchTasks(users, keyword)
	if DebugLog {
		fmt.Printf("[Gying] 搜索完成，获得 %d 条结果\n", len(results))
	}
	realResult := model.PluginSearchResult{
		Results: results,
		IsFinal: true,
	}
	// 写入缓存
	if len(results) > 0 {
		p.searchCache.Store(keyword, realResult)
	}
	return realResult, nil
}

// ============ 用户管理 ============

// loadAllUsers 加载所有用户到内存（包括用户名、加密密码、cookie快照等）
// scraper实例会在初始化阶段优先用已保存的cookie恢复，失败后再回退到密码重登。
func (p *GyingPlugin) loadAllUsers() {
	files, err := ioutil.ReadDir(StorageDir)
	if err != nil {
		return
	}

	totalFiles := 0
	loadedCount := 0
	skippedInactive := 0

	for _, file := range files {
		if file.IsDir() || filepath.Ext(file.Name()) != ".json" || file.Name() == GyingConfigFileName {
			continue
		}

		totalFiles++

		filePath := filepath.Join(StorageDir, file.Name())
		data, err := ioutil.ReadFile(filePath)
		if err != nil {
			continue
		}

		var user User
		if err := json.Unmarshal(data, &user); err != nil {
			continue
		}

		// 过滤条件：status必须是active
		if user.Status != "active" {
			if DebugLog {
				fmt.Printf("[Gying] ⏭️  跳过用户 %s: status=%s (非active)\n", user.Username, user.Status)
			}
			skippedInactive++
			continue
		}

		// 只存储用户数据（包括用户名和加密密码）
		// scraper实例将在initDefaultAccounts中通过重新登录获取
		p.users.Store(user.Hash, &user)
		loadedCount++

		if DebugLog {
			hasPassword := "无"
			if user.EncryptedPassword != "" {
				hasPassword = "有"
			}
			fmt.Printf("[Gying] ✅ 已加载用户 %s (密码:%s, 将在初始化时登录)\n", user.Username, hasPassword)
		}
	}

	fmt.Printf("[Gying] 用户加载完成: 总文件=%d, 已加载=%d, 跳过(非active)=%d\n",
		totalFiles, loadedCount, skippedInactive)
}

// initDefaultAccounts 初始化所有账户（异步执行，不阻塞启动）
// 包括：1. DefaultAccounts（代码配置）  2. 从文件加载的用户（优先恢复cookie会话，失败后再使用加密密码重新登录）
func (p *GyingPlugin) initDefaultAccounts() {
	// fmt.Printf("[Gying] ========== 异步初始化所有账户 ==========\n")

	// 步骤1：处理DefaultAccounts（代码中配置的默认账户）
	for i, account := range DefaultAccounts {
		if DebugLog {
			fmt.Printf("[Gying] [默认账户 %d/%d] 处理: %s\n", i+1, len(DefaultAccounts), account.Username)
		}

		p.initOrRestoreUser(account.Username, account.Password, "default")
	}

	// 步骤2：遍历所有已加载的用户，恢复没有scraper的用户
	var usersToRestore []*User
	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)
		// 检查scraper是否存在
		_, scraperExists := p.scrapers.Load(user.Hash)
		if !scraperExists && user.EncryptedPassword != "" {
			usersToRestore = append(usersToRestore, user)
		}
		return true
	})

	if len(usersToRestore) > 0 {
		fmt.Printf("[Gying] 发现 %d 个需要恢复的用户（使用加密密码重新登录）\n", len(usersToRestore))
		for i, user := range usersToRestore {
			if DebugLog {
				fmt.Printf("[Gying] [恢复用户 %d/%d] 处理: %s\n", i+1, len(usersToRestore), user.Username)
			}

			// 解密密码
			password, err := p.decryptPassword(user.EncryptedPassword)
			if err != nil {
				fmt.Printf("[Gying] ❌ 用户 %s 解密密码失败: %v\n", user.Username, err)
				continue
			}

			p.initOrRestoreUser(user.Username, password, "restore")
		}
	}

	// fmt.Printf("[Gying] ========== 所有账户初始化完成 ==========\n")
}

// initOrRestoreUser 初始化或恢复单个用户（登录并保存）
func (p *GyingPlugin) initOrRestoreUser(username, password, source string) {
	hash := p.generateHash(username)

	// 检查scraper是否已存在
	_, scraperExists := p.scrapers.Load(hash)
	if scraperExists {
		if DebugLog {
			fmt.Printf("[Gying] 用户 %s scraper已存在，跳过\n", username)
		}
		return
	}

	if existingUser, exists := p.getUserByHash(hash); exists && existingUser.Cookie != "" {
		scraper, err := p.createScraperWithCookies(existingUser.Cookie)
		if err == nil {
			existingUser.LastAccessAt = time.Now()
			p.scrapers.Store(hash, scraper)
			if err := p.saveUser(existingUser); err != nil && DebugLog {
				fmt.Printf("[Gying] ⚠️  恢复用户 %s cookie会话后保存失败: %v\n", username, err)
			}
			fmt.Printf("[Gying] ✅ 账户 %s 会话恢复成功 (来源:%s, 使用已保存cookie)\n", username, source)
			return
		}

		if DebugLog {
			fmt.Printf("[Gying] ⚠️  账户 %s cookie会话恢复失败，回退到密码登录: %v\n", username, err)
		}
	}

	// 登录
	if DebugLog {
		fmt.Printf("[Gying] 开始登录账户: %s\n", username)
	}
	scraper, cookie, err := p.doLogin(username, password)
	if err != nil {
		fmt.Printf("[Gying] ❌ 账户 %s 登录失败: %v\n", username, err)
		return
	}

	if DebugLog {
		fmt.Printf("[Gying] 登录成功，已获取cloudscraper实例\n")
	}

	// 加密密码
	encryptedPassword, err := p.encryptPassword(password)
	if err != nil {
		fmt.Printf("[Gying] ❌ 加密密码失败: %v\n", err)
		return
	}

	// 保存用户
	user := &User{
		Hash:              hash,
		Username:          username,
		EncryptedPassword: encryptedPassword,
		Cookie:            cookie,
		Status:            "active",
		CreatedAt:         time.Now(),
		LoginAt:           time.Now(),
		ExpireAt:          time.Now().AddDate(0, 4, 0), // 121天有效期
		LastAccessAt:      time.Now(),
	}

	// 保存scraper实例到内存
	p.scrapers.Store(hash, scraper)

	if err := p.saveUser(user); err != nil {
		fmt.Printf("[Gying] ❌ 保存账户失败: %v\n", err)
		return
	}

	fmt.Printf("[Gying] ✅ 账户 %s 初始化成功 (来源:%s)\n", user.Username, source)
}

// getUserByHash 获取用户
func (p *GyingPlugin) getUserByHash(hash string) (*User, bool) {
	value, ok := p.users.Load(hash)
	if !ok {
		return nil, false
	}
	return value.(*User), true
}

// saveUser 保存用户
func (p *GyingPlugin) saveUser(user *User) error {
	p.users.Store(user.Hash, user)
	return p.persistUser(user)
}

func (p *GyingPlugin) syncUserCookiesFromScraper(user *User, scraper *cloudscraper.Scraper) error {
	if user == nil || scraper == nil {
		return nil
	}

	cookieStr, err := p.exportCookies(scraper, p.getBaseURL())
	if err != nil {
		return err
	}
	if cookieStr == "" || cookieStr == user.Cookie {
		return nil
	}

	user.Cookie = cookieStr
	user.LastAccessAt = time.Now()
	return p.saveUser(user)
}

// persistUser 持久化用户到文件
func (p *GyingPlugin) persistUser(user *User) error {
	filePath := filepath.Join(StorageDir, user.Hash+".json")
	data, err := json.MarshalIndent(user, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filePath, data, 0644)
}

// deleteUser 删除用户
func (p *GyingPlugin) deleteUser(hash string) error {
	p.users.Delete(hash)
	filePath := filepath.Join(StorageDir, hash+".json")
	return os.Remove(filePath)
}

// getActiveUsers 获取有效用户
func (p *GyingPlugin) getActiveUsers() []*User {
	var users []*User

	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)
		if user.Status == "active" && user.Cookie != "" {
			users = append(users, user)
		}
		return true
	})

	return users
}

// ============ HTTP路由处理 ============

// handleManagePage GET路由处理
func (p *GyingPlugin) handleManagePage(c *gin.Context) {
	param := c.Param("param")

	// 判断是用户名还是hash
	if len(param) == 64 && p.isHexString(param) {
		html := strings.ReplaceAll(HTMLTemplate, "HASH_PLACEHOLDER", param)
		c.Data(200, "text/html; charset=utf-8", []byte(html))
	} else {
		hash := p.generateHash(param)
		c.Redirect(302, "/gying/"+hash)
	}
}

// handleManagePagePOST POST路由处理
func (p *GyingPlugin) handleManagePagePOST(c *gin.Context) {
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
	case "get_config":
		p.handleGetConfig(c)
	case "login":
		p.handleLogin(c, hash, reqData)
	case "logout":
		p.handleLogout(c, hash)
	case "update_config":
		p.handleUpdateConfig(c, reqData)
	case "test_search":
		p.handleTestSearch(c, hash, reqData)
	default:
		respondError(c, "未知的操作类型: "+action)
	}
}

// handleGetStatus 获取状态
func (p *GyingPlugin) handleGetStatus(c *gin.Context, hash string) {
	user, exists := p.getUserByHash(hash)

	if !exists {
		user = &User{
			Hash:         hash,
			Status:       "pending",
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
		"username":        user.Username,
		"login_time":      user.LoginAt.Format("2006-01-02 15:04:05"),
		"expire_time":     user.ExpireAt.Format("2006-01-02 15:04:05"),
		"expires_in_days": expiresInDays,
	})
}

// handleGetConfig 获取站点配置
func (p *GyingPlugin) handleGetConfig(c *gin.Context) {
	respondSuccess(c, "获取成功", gin.H{
		"base_url": p.getBaseURL(),
	})
}

// handleUpdateConfig 更新站点配置
func (p *GyingPlugin) handleUpdateConfig(c *gin.Context, reqData map[string]interface{}) {
	baseURL, _ := reqData["base_url"].(string)
	if strings.TrimSpace(baseURL) == "" {
		respondError(c, "缺少站点地址")
		return
	}

	savedBaseURL, err := p.updateBaseURL(baseURL)
	if err != nil {
		respondError(c, "保存站点地址失败: "+err.Error())
		return
	}

	respondSuccess(c, "站点地址已保存，当前登录状态已清空，请重新登录", gin.H{
		"base_url": savedBaseURL,
	})
}

// handleLogin 处理登录
func (p *GyingPlugin) handleLogin(c *gin.Context, hash string, reqData map[string]interface{}) {
	username, _ := reqData["username"].(string)
	password, _ := reqData["password"].(string)

	if username == "" || password == "" {
		respondError(c, "缺少用户名或密码")
		return
	}

	// 执行登录
	scraper, cookie, err := p.doLogin(username, password)
	if err != nil {
		respondError(c, "登录失败: "+err.Error())
		return
	}

	// 保存scraper实例到内存
	p.scrapers.Store(hash, scraper)

	// 加密密码
	encryptedPassword, err := p.encryptPassword(password)
	if err != nil {
		respondError(c, "加密密码失败: "+err.Error())
		return
	}

	// 保存用户
	user := &User{
		Hash:              hash,
		Username:          username,
		EncryptedPassword: encryptedPassword,
		Cookie:            cookie,
		Status:            "active",
		LoginAt:           time.Now(),
		ExpireAt:          time.Now().AddDate(0, 4, 0), // 121天
		LastAccessAt:      time.Now(),
	}

	if _, exists := p.getUserByHash(hash); !exists {
		user.CreatedAt = time.Now()
	}

	if err := p.saveUser(user); err != nil {
		respondError(c, "保存失败: "+err.Error())
		return
	}

	respondSuccess(c, "登录成功", gin.H{
		"status":   "active",
		"username": user.Username,
	})
}

// handleLogout 退出登录
func (p *GyingPlugin) handleLogout(c *gin.Context, hash string) {
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

// handleTestSearch 测试搜索
func (p *GyingPlugin) handleTestSearch(c *gin.Context, hash string, reqData map[string]interface{}) {
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

	// 获取scraper实例
	scraperVal, exists := p.scrapers.Load(hash)
	if !exists {
		respondError(c, "用户scraper实例不存在，请重新登录")
		return
	}

	scraper, ok := scraperVal.(*cloudscraper.Scraper)
	if !ok || scraper == nil {
		respondError(c, "scraper实例无效，请重新登录")
		return
	}

	// 执行搜索（带403自动重新登录）
	results, err := p.searchWithScraperWithRetry(keyword, scraper, user)
	if err != nil {
		respondError(c, "搜索失败: "+err.Error())
		return
	}

	// 限制返回数量
	maxResults := 10
	if len(results) > maxResults {
		results = results[:maxResults]
	}

	// 转换为前端格式
	frontendResults := make([]gin.H, 0, len(results))
	for _, r := range results {
		links := make([]gin.H, 0, len(r.Links))
		for _, link := range r.Links {
			links = append(links, gin.H{
				"type":       link.Type,
				"url":        link.URL,
				"password":   link.Password,
				"work_title": link.WorkTitle,
			})
		}

		frontendResults = append(frontendResults, gin.H{
			"title": r.Title,
			"links": links,
		})
	}

	respondSuccess(c, fmt.Sprintf("找到 %d 条结果", len(frontendResults)), gin.H{
		"keyword":       keyword,
		"total_results": len(frontendResults),
		"results":       frontendResults,
	})
}

// ============ 密码加密/解密 ============

// encryptPassword 使用AES加密密码
func (p *GyingPlugin) encryptPassword(password string) (string, error) {
	// 使用固定密钥（实际应用中可以使用配置或环境变量）
	key := []byte("gying-secret-key-32bytes-long!!!") // 32字节密钥用于AES-256

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	// 创建GCM模式
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	// 生成随机nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	// 加密
	ciphertext := gcm.Seal(nonce, nonce, []byte(password), nil)

	// 返回base64编码的密文
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decryptPassword 解密密码
func (p *GyingPlugin) decryptPassword(encrypted string) (string, error) {
	// 使用与加密相同的密钥
	key := []byte("gying-secret-key-32bytes-long!!!")

	// base64解码
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

// ============ Cookie管理 ============

// ============ Cookie 与反爬处理 ============

// getScraperClient 通过反射拿到 cloudscraper 内部的 http.Client，
// 便于读取和回写 cookie jar。
func getScraperClient(scraper *cloudscraper.Scraper) (*http.Client, error) {
	if scraper == nil {
		return nil, fmt.Errorf("scraper 实例为空")
	}

	scraperValue := reflect.ValueOf(scraper)
	if scraperValue.Kind() != reflect.Ptr || scraperValue.IsNil() {
		return nil, fmt.Errorf("scraper 实例无效")
	}

	clientField := scraperValue.Elem().FieldByName("client")
	if !clientField.IsValid() || clientField.IsNil() {
		return nil, fmt.Errorf("未找到 scraper client")
	}

	clientValue := reflect.NewAt(clientField.Type(), unsafe.Pointer(clientField.UnsafeAddr())).Elem()
	client, ok := clientValue.Interface().(*http.Client)
	if !ok || client == nil {
		return nil, fmt.Errorf("scraper client 无效")
	}

	return client, nil
}

func isBotChallengePage(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	bodyText := string(body)
	hasVerifyText := strings.Contains(bodyText, "正在确认你是不是机器人") ||
		strings.Contains(bodyText, "浏览器安全验证") ||
		strings.Contains(bodyText, "安全验证") ||
		strings.Contains(bodyText, "正在进行浏览器计算验证")
	if !hasVerifyText {
		return false
	}

	return challengeJSONPattern.Match(body) ||
		strings.Contains(bodyText, "powSolve-") ||
		strings.Contains(bodyText, "pow.worker-") ||
		strings.Contains(bodyText, "const jss=") ||
		strings.Contains(bodyText, "/res/pow")
}

func isLoginShell(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	bodyText := string(body)
	return strings.Contains(bodyText, "_BT.PC.HTML('login')") ||
		strings.Contains(bodyText, `_BT.PC.HTML("login")`) ||
		strings.Contains(bodyText, "_BT.PC.HTML('nologin')") ||
		strings.Contains(bodyText, `_BT.PC.HTML("nologin")`) ||
		strings.Contains(bodyText, "未登录，访问受限")
}

func (p *GyingPlugin) exportCookies(scraper *cloudscraper.Scraper, rawURL string) (string, error) {
	client, err := getScraperClient(scraper)
	if err != nil {
		return "", err
	}
	if client.Jar == nil {
		return "", fmt.Errorf("scraper cookie jar 为空")
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	cookies := client.Jar.Cookies(parsedURL)
	if len(cookies) == 0 {
		return "", nil
	}

	sort.Slice(cookies, func(i, j int) bool {
		return cookies[i].Name < cookies[j].Name
	})

	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		parts = append(parts, fmt.Sprintf("%s=%s", cookie.Name, cookie.Value))
	}

	return strings.Join(parts, "; "), nil
}

func collectSetCookies(headers http.Header, cookieMap map[string]string) {
	for _, setCookie := range headers.Values("Set-Cookie") {
		parts := strings.Split(setCookie, ";")
		if len(parts) == 0 {
			continue
		}

		cookiePart := strings.TrimSpace(parts[0])
		if idx := strings.Index(cookiePart, "="); idx > 0 {
			name := cookiePart[:idx]
			value := cookiePart[idx+1:]
			cookieMap[name] = value
		}
	}
}

func logSetCookiesForDebug(headers http.Header, prefix string, maxLen int) {
	if !DebugLog {
		return
	}

	for _, setCookie := range headers.Values("Set-Cookie") {
		parts := strings.Split(setCookie, ";")
		if len(parts) == 0 {
			continue
		}

		cookiePart := strings.TrimSpace(parts[0])
		if idx := strings.Index(cookiePart, "="); idx > 0 {
			name := cookiePart[:idx]
			value := cookiePart[idx+1:]
			displayValue := value
			if maxLen > 0 && len(displayValue) > maxLen {
				displayValue = displayValue[:maxLen] + "..."
			}
			fmt.Printf(prefix, name, displayValue)
		}
	}
}

func (p *GyingPlugin) applyProxyToScraper(scraper *cloudscraper.Scraper) error {
	if scraper == nil {
		return fmt.Errorf("scraper 实例为空")
	}
	if config.AppConfig == nil || strings.TrimSpace(config.AppConfig.ProxyURL) == "" {
		return nil
	}

	client, err := getScraperClient(scraper)
	if err != nil {
		return err
	}
	if client.Transport == nil {
		return fmt.Errorf("scraper transport 为空")
	}

	transportValue := reflect.ValueOf(client.Transport)
	if transportValue.Kind() != reflect.Ptr || transportValue.IsNil() {
		return fmt.Errorf("scraper transport 无效")
	}

	transportElem := transportValue.Elem()
	baseTransportField := transportElem.FieldByName("Transport")
	if !baseTransportField.IsValid() || baseTransportField.IsNil() {
		return fmt.Errorf("未找到底层 transport")
	}

	baseTransportValue := reflect.NewAt(baseTransportField.Type(), unsafe.Pointer(baseTransportField.UnsafeAddr())).Elem()
	baseTransport, ok := baseTransportValue.Interface().(*http.Transport)
	if !ok || baseTransport == nil {
		return fmt.Errorf("底层 transport 无效")
	}

	proxyURL, err := url.Parse(config.AppConfig.ProxyURL)
	if err != nil {
		return fmt.Errorf("解析代理地址失败: %w", err)
	}

	if proxyURL.Scheme == "socks5" {
		dialer, err := proxy.FromURL(proxyURL, proxy.Direct)
		if err != nil {
			return fmt.Errorf("创建SOCKS5代理失败: %w", err)
		}
		baseTransport.Proxy = nil
		baseTransport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		}
	} else {
		baseTransport.Proxy = http.ProxyURL(proxyURL)
	}

	if DebugLog {
		fmt.Printf("[Gying] 已应用代理到scraper: %s\n", config.AppConfig.ProxyURL)
	}

	return nil
}

func (p *GyingPlugin) solveBotChallenge(scraper *cloudscraper.Scraper, requestURL string, body []byte) error {
	matches := challengeJSONPattern.FindSubmatch(body)
	if len(matches) < 2 {
		return p.solveRemotePowChallenge(scraper, requestURL)
	}

	var challenge ChallengePageData
	if err := json.Unmarshal(matches[1], &challenge); err != nil {
		return fmt.Errorf("解析验证数据失败: %w", err)
	}

	if challenge.ID != "" && challenge.N != "" && challenge.X != "" && challenge.T > 0 {
		return p.solvePowChallenge(scraper, requestURL, &challenge)
	}

	return p.solveLegacyHashChallenge(scraper, requestURL, &challenge)
}

func (p *GyingPlugin) solveRemotePowChallenge(scraper *cloudscraper.Scraper, requestURL string) error {
	powURL, err := p.buildPowURL(requestURL)
	if err != nil {
		return err
	}

	if DebugLog {
		fmt.Printf("[Gying] Remote PoW Challenge命中: url=%s powURL=%s\n", requestURL, powURL)
	}

	body, statusCode, _, err := p.requestWithChallengeRetry(scraper, http.MethodGet, powURL, "", "")
	if err != nil {
		return fmt.Errorf("获取PoW验证数据失败: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("获取PoW验证数据失败: HTTP %d", statusCode)
	}

	var challenge ChallengePageData
	if err := json.Unmarshal(body, &challenge); err != nil {
		return fmt.Errorf("解析PoW验证数据失败: %w", err)
	}
	if challenge.N == "" || challenge.X == "" || challenge.T <= 0 {
		return fmt.Errorf("PoW验证数据无效")
	}

	y, err := p.computePowResult(&challenge)
	if err != nil {
		return err
	}

	form := url.Values{}
	form.Set("y", y)

	return p.submitChallengeVerification(scraper, powURL, form)
}

func (p *GyingPlugin) buildPowURL(requestURL string) (string, error) {
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return "", fmt.Errorf("解析验证URL失败: %w", err)
	}
	parsedURL.Path = "/res/pow"
	parsedURL.RawQuery = ""
	parsedURL.Fragment = ""
	return parsedURL.String(), nil
}

func (p *GyingPlugin) solvePowChallenge(scraper *cloudscraper.Scraper, requestURL string, challenge *ChallengePageData) error {
	y, err := p.computePowResult(challenge)
	if err != nil {
		return err
	}

	form := url.Values{}
	form.Set("action", "verify")
	form.Set("id", challenge.ID)
	form.Set("y", y)

	return p.submitChallengeVerification(scraper, requestURL, form)
}

func (p *GyingPlugin) computePowResult(challenge *ChallengePageData) (string, error) {
	modulus, ok := new(big.Int).SetString(challenge.N, 16)
	if !ok || modulus.Sign() <= 0 {
		return "", fmt.Errorf("PoW验证数据无效: N")
	}

	y, ok := new(big.Int).SetString(challenge.X, 16)
	if !ok || y.Sign() < 0 {
		return "", fmt.Errorf("PoW验证数据无效: x")
	}
	if challenge.T <= 0 {
		return "", fmt.Errorf("PoW验证数据无效: t")
	}

	if DebugLog {
		fmt.Printf("[Gying] PoW Challenge计算开始: id=%s t=%d nBits=%d\n",
			challenge.ID, challenge.T, modulus.BitLen())
	}

	start := time.Now()
	for i := 0; i < challenge.T; i++ {
		y.Mul(y, y)
		y.Mod(y, modulus)
	}
	elapsed := time.Since(start)

	if DebugLog {
		fmt.Printf("[Gying] PoW Challenge计算完成: id=%s t=%d cost=%s\n",
			challenge.ID, challenge.T, elapsed.Round(time.Millisecond))
	}

	if minSolveTime := 3 * time.Second; elapsed < minSolveTime {
		time.Sleep(minSolveTime - elapsed)
	}

	return y.Text(16), nil
}

func (p *GyingPlugin) solveLegacyHashChallenge(scraper *cloudscraper.Scraper, requestURL string, challenge *ChallengePageData) error {
	if challenge.ID == "" || challenge.Salt == "" || challenge.Diff <= 0 || len(challenge.Challenge) == 0 {
		return fmt.Errorf("验证数据无效")
	}

	if DebugLog {
		fmt.Printf("[Gying] Challenge命中: url=%s id=%s diff=%d targets=%d\n",
			requestURL, challenge.ID, challenge.Diff, len(challenge.Challenge))
	}

	remaining := make(map[string][]int, len(challenge.Challenge))
	nonces := make([]int, len(challenge.Challenge))
	for idx, target := range challenge.Challenge {
		hash := strings.ToLower(target)
		remaining[hash] = append(remaining[hash], idx)
	}

	workerCount := runtime.GOMAXPROCS(0)
	if workerCount < 1 {
		workerCount = 1
	}
	if maxWorkers := challenge.Diff + 1; workerCount > maxWorkers {
		workerCount = maxWorkers
	}

	var (
		mu         sync.Mutex
		solved     atomic.Int32
		targetsLen = int32(len(challenge.Challenge))
		wg         sync.WaitGroup
		saltBytes  = []byte(challenge.Salt)
	)

	for workerID := 0; workerID < workerCount; workerID++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()

			hashInput := make([]byte, 0, len(saltBytes)+20)
			for nonce := start; nonce <= challenge.Diff; nonce += workerCount {
				if solved.Load() >= targetsLen {
					return
				}

				hashInput = strconv.AppendInt(hashInput[:0], int64(nonce), 10)
				hashInput = append(hashInput, saltBytes...)

				sum := sha256.Sum256(hashInput)
				hash := hex.EncodeToString(sum[:])

				mu.Lock()
				indexes := remaining[hash]
				if len(indexes) > 0 {
					idx := indexes[0]
					nonces[idx] = nonce
					solved.Add(1)
					if len(indexes) == 1 {
						delete(remaining, hash)
					} else {
						remaining[hash] = indexes[1:]
					}
					if solved.Load() >= targetsLen {
						mu.Unlock()
						return
					}
				}
				mu.Unlock()
			}
		}(workerID)
	}

	wg.Wait()

	if solved.Load() != targetsLen {
		mu.Lock()
		missing := len(remaining)
		mu.Unlock()
		if missing > 0 {
			return fmt.Errorf("无法完成机器人验证")
		}
	}

	form := url.Values{}
	form.Set("action", "verify")
	form.Set("id", challenge.ID)
	for _, nonce := range nonces {
		form.Add("nonce[]", strconv.Itoa(nonce))
	}

	return p.submitChallengeVerification(scraper, requestURL, form)
}

func (p *GyingPlugin) submitChallengeVerification(scraper *cloudscraper.Scraper, requestURL string, form url.Values) error {
	resp, err := scraper.Post(requestURL, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("提交验证失败: %w", err)
	}
	defer resp.Body.Close()

	if DebugLog {
		fmt.Printf("[Gying] Challenge提交完成: url=%s status=%d\n", requestURL, resp.StatusCode)
	}

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取验证响应失败: %w", err)
	}
	if isBotChallengePage(respBody) {
		return fmt.Errorf("机器人验证出现循环")
	}

	var verifyResp challengeVerifyResponse
	if err := json.Unmarshal(respBody, &verifyResp); err != nil {
		return fmt.Errorf("解析验证响应失败: %w", err)
	}
	if !verifyResp.Success {
		if verifyResp.Msg != "" {
			return fmt.Errorf("机器人验证失败: %s", verifyResp.Msg)
		}
		return fmt.Errorf("机器人验证失败")
	}

	if DebugLog {
		fmt.Printf("[Gying] Challenge验证成功: url=%s\n", requestURL)
	}

	return nil
}

func (p *GyingPlugin) requestWithChallengeRetry(scraper *cloudscraper.Scraper, method, requestURL, contentType, requestBody string) ([]byte, int, http.Header, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var (
			resp *http.Response
			err  error
		)

		switch method {
		case http.MethodGet:
			// Use cloudscraper for GET as well as POST so the challenge page,
			// verification request, and retry share the same browser-like headers.
			resp, err = scraper.Get(requestURL)
		case http.MethodPost:
			resp, err = scraper.Post(requestURL, contentType, strings.NewReader(requestBody))
		default:
			return nil, 0, nil, fmt.Errorf("不支持的请求方法: %s", method)
		}
		if err != nil {
			return nil, 0, nil, err
		}

		statusCode := resp.StatusCode
		headers := resp.Header.Clone()
		body, readErr := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, statusCode, headers, readErr
		}

		if DebugLog {
			fmt.Printf("[Gying] requestWithChallengeRetry: method=%s attempt=%d status=%d url=%s bodyLen=%d challenge=%v loginShell=%v\n",
				method, attempt+1, statusCode, requestURL, len(body), isBotChallengePage(body), isLoginShell(body))
		}

		if isBotChallengePage(body) {
			if attempt == 1 {
				return nil, statusCode, headers, fmt.Errorf("重试后仍然进入机器人验证页")
			}
			if DebugLog {
				fmt.Printf("[Gying] requestWithChallengeRetry: 准备求解challenge并重试 url=%s\n", requestURL)
			}
			if err := p.solveBotChallenge(scraper, requestURL, body); err != nil {
				return nil, statusCode, headers, err
			}
			if DebugLog {
				fmt.Printf("[Gying] requestWithChallengeRetry: challenge已完成，重试原请求 url=%s\n", requestURL)
			}
			continue
		}

		return body, statusCode, headers, nil
	}

	return nil, 0, nil, fmt.Errorf("请求重试次数已耗尽")
}

// createScraperWithCookies 创建一个带有指定cookies的cloudscraper实例
// 使用反射访问内部的http.Client并设置cookies到cookiejar
// 关键：禁用session refresh以防止cookies被清空
func (p *GyingPlugin) createScraperWithCookies(cookieStr string) (*cloudscraper.Scraper, error) {
	// 创建cloudscraper实例，配置以保护cookies不被刷新
	scraper, err := cloudscraper.New(
		cloudscraper.WithSessionConfig(
			false,            // refreshOn403 = false，禁用403时自动刷新
			365*24*time.Hour, // interval = 1年，基本不刷新
			0,                // maxRetries = 0
		),
	)
	if err != nil {
		return nil, fmt.Errorf("创建cloudscraper失败: %w", err)
	}
	if err := p.applyProxyToScraper(scraper); err != nil {
		return nil, fmt.Errorf("应用代理失败: %w", err)
	}

	// 如果有保存的cookies，使用反射设置到scraper的内部http.Client
	if cookieStr != "" {
		cookies := parseCookieString(cookieStr)

		if DebugLog {
			fmt.Printf("[Gying] 正在恢复 %d 个cookie到scraper实例\n", len(cookies))
		}

		// 使用反射访问scraper的unexported client字段
		scraperValue := reflect.ValueOf(scraper).Elem()
		clientField := scraperValue.FieldByName("client")

		if clientField.IsValid() && !clientField.IsNil() {
			// 使用反射访问client (需要使用Elem()因为是指针)
			clientValue := reflect.NewAt(clientField.Type(), unsafe.Pointer(clientField.UnsafeAddr())).Elem()
			client, ok := clientValue.Interface().(*http.Client)

			if ok && client != nil && client.Jar != nil {
				// 将cookies设置到cookiejar
				// 注意：必须使用正确的URL和cookie属性
				gyingURL, _ := url.Parse(p.getBaseURL())
				var httpCookies []*http.Cookie

				for name, value := range cookies {
					cookie := &http.Cookie{
						Name:  name,
						Value: value,
						// 不设置Domain和Path，让cookiejar根据URL自动推导
						// cookiejar.SetCookies会根据提供的URL自动设置正确的Domain和Path
					}
					httpCookies = append(httpCookies, cookie)

					if DebugLog {
						fmt.Printf("[Gying]   准备恢复Cookie: %s=%s\n",
							cookie.Name, cookie.Value[:min(10, len(cookie.Value))])
					}
				}

				client.Jar.SetCookies(gyingURL, httpCookies)

				// 验证cookies是否被正确设置
				if DebugLog {
					storedCookies := client.Jar.Cookies(gyingURL)
					fmt.Printf("[Gying] ✅ 成功恢复 %d 个cookie到scraper的cookiejar\n", len(cookies))
					fmt.Printf("[Gying] 验证: cookiejar中现有 %d 个cookie\n", len(storedCookies))

					// 详细打印每个cookie以便调试
					for i, c := range storedCookies {
						fmt.Printf("[Gying]   设置后Cookie[%d]: %s=%s (Domain:%s, Path:%s)\n",
							i, c.Name, c.Value[:min(10, len(c.Value))], c.Domain, c.Path)
					}
				}
			} else {
				if DebugLog {
					fmt.Printf("[Gying] ⚠️  无法获取http.Client或其Jar\n")
				}
			}
		} else {
			if DebugLog {
				fmt.Printf("[Gying] ⚠️  无法通过反射访问client字段\n")
			}
		}
	}

	return scraper, nil
}

// parseCookieString 解析cookie字符串为map
func parseCookieString(cookieStr string) map[string]string {
	cookies := make(map[string]string)
	parts := strings.Split(cookieStr, ";")

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if idx := strings.Index(part, "="); idx > 0 {
			name := part[:idx]
			value := part[idx+1:]
			cookies[name] = value
		}
	}

	return cookies
}

// ============ 登录逻辑 ============

// doLogin 执行登录，返回scraper实例和cookie字符串
//
// 登录流程（3步）：
//  1. GET登录页 (https://www.gying.net/user/login/) → 获取PHPSESSID
//  2. POST登录  (https://www.gying.net/user/login)  → 获取BT_auth、BT_cookietime等认证cookies
//  3. GET详情页 (https://www.gying.net/mv/wkMn)     → 触发防爬cookies (vrg_sc、vrg_go等)
//
// 返回: (*cloudscraper.Scraper, cookie字符串, error)
func (p *GyingPlugin) doLogin(username, password string) (*cloudscraper.Scraper, string, error) {
	if DebugLog {
		fmt.Printf("[Gying] ========== 开始登录 ==========\n")
		fmt.Printf("[Gying] 用户名: %s\n", username)
		fmt.Printf("[Gying] 密码长度: %d\n", len(password))
	}

	// 创建cloudscraper实例（每个用户独立的实例）
	// 关键配置：禁用403自动刷新,防止cookie被清空
	scraper, err := cloudscraper.New(
		cloudscraper.WithSessionConfig(
			false,            // refreshOn403 = false，禁用403时自动刷新（重要！）
			365*24*time.Hour, // interval = 1年，基本不刷新
			0,                // maxRetries = 0
		),
	)
	if err != nil {
		if DebugLog {
			fmt.Printf("[Gying] 创建cloudscraper失败: %v\n", err)
		}
		return nil, "", fmt.Errorf("创建cloudscraper失败: %w", err)
	}
	if err := p.applyProxyToScraper(scraper); err != nil {
		if DebugLog {
			fmt.Printf("[Gying] 应用代理失败: %v\n", err)
		}
		return nil, "", fmt.Errorf("应用代理失败: %w", err)
	}

	if DebugLog {
		fmt.Printf("[Gying] cloudscraper创建成功（已禁用403自动刷新）\n")
	}

	// 创建cookieMap用于收集所有cookies
	cookieMap := make(map[string]string)

	// ========== 步骤1: GET登录页 (获取初始PHPSESSID) ==========
	loginPageURL := p.getLoginPageURL()
	if DebugLog {
		fmt.Printf("[Gying] 步骤1: 访问登录页面: %s\n", loginPageURL)
	}

	_, statusCode, headers, err := p.requestWithChallengeRetry(scraper, http.MethodGet, loginPageURL, "", "")

	if err != nil {
		if DebugLog {
			fmt.Printf("[Gying] 访问登录页面失败: %v\n", err)
		}
		return nil, "", fmt.Errorf("访问登录页面失败: %w", err)
	}

	if DebugLog {
		fmt.Printf("[Gying] 登录页面状态码: %d\n", statusCode)
	}

	if statusCode != http.StatusOK {
		return nil, "", fmt.Errorf("访问登录页面失败: HTTP %d", statusCode)
	}
	// 从登录页响应中收集cookies
	collectSetCookies(headers, cookieMap)
	logSetCookiesForDebug(headers, "[Gying]   登录页Cookie: %s=%s\n", 20)

	// ========== 步骤2: POST登录 (获取认证cookies) ==========
	loginURL := p.getLoginAPIURL()
	postData := fmt.Sprintf("code=&siteid=1&dosubmit=1&cookietime=10506240&username=%s&password=%s",
		url.QueryEscape(username),
		url.QueryEscape(password))

	if DebugLog {
		fmt.Printf("[Gying] 步骤2: POST登录\n")
		fmt.Printf("[Gying] 登录URL: %s\n", loginURL)
		fmt.Printf("[Gying] POST数据: code=&siteid=1&dosubmit=1&cookietime=10506240&username=%s&password=<len:%d>\n",
			url.QueryEscape(username), len(password))
	}

	body, statusCode, headers, err := p.requestWithChallengeRetry(scraper, http.MethodPost, loginURL, "application/x-www-form-urlencoded", postData)
	if err != nil {
		if DebugLog {
			fmt.Printf("[Gying] 登录POST请求失败: %v\n", err)
		}
		return nil, "", fmt.Errorf("登录POST请求失败: %w", err)
	}
	// 从POST登录响应中收集cookies
	collectSetCookies(headers, cookieMap)
	logSetCookiesForDebug(headers, "[Gying]   POST登录Cookie: %s=%s\n", 20)

	if DebugLog {
		fmt.Printf("[Gying] 响应状态码: %d\n", statusCode)
	}

	// 读取响应
	if DebugLog {
		fmt.Printf("[Gying] 响应内容: %s\n", string(body))
	}

	var loginResp map[string]interface{}
	if err := json.Unmarshal(body, &loginResp); err != nil {
		if DebugLog {
			fmt.Printf("[Gying] JSON解析失败: %v\n", err)
		}
		return nil, "", fmt.Errorf("JSON解析失败: %w, 响应内容: %s", err, string(body))
	}

	if DebugLog {
		fmt.Printf("[Gying] 解析后的响应: %+v\n", loginResp)
		fmt.Printf("[Gying] code字段类型: %T, 值: %v\n", loginResp["code"], loginResp["code"])
	}

	// 检查登录结果（兼容多种类型：int、float64、json.Number、string）
	var codeValue int
	codeInterface := loginResp["code"]

	switch v := codeInterface.(type) {
	case int:
		codeValue = v
	case float64:
		codeValue = int(v)
	case int64:
		codeValue = int(v)
	default:
		// 尝试转换为字符串再解析
		codeStr := fmt.Sprintf("%v", codeInterface)
		parsed, err := strconv.Atoi(codeStr)
		if err != nil {
			if DebugLog {
				fmt.Printf("[Gying] 无法解析code字段: %T, 值: %v, 错误: %v\n", codeInterface, codeInterface, err)
			}
			return nil, "", fmt.Errorf("无法解析code字段，类型: %T, 值: %v", codeInterface, codeInterface)
		}
		codeValue = parsed
	}

	if DebugLog {
		fmt.Printf("[Gying] 解析后的code值: %d\n", codeValue)
	}

	if codeValue != 200 {
		if DebugLog {
			fmt.Printf("[Gying] 登录失败: code=%d (期望200)\n", codeValue)
		}
		return nil, "", fmt.Errorf("登录失败: code=%d, 响应=%s", codeValue, string(body))
	}

	// ========== 步骤3: GET详情页 (触发防爬cookies如vrg_sc、vrg_go等) ==========
	if DebugLog {
		fmt.Printf("[Gying] 步骤3: GET详情页收集完整Cookie\n")
	}

	_, warmupStatus, warmupHeaders, err := p.requestWithChallengeRetry(scraper, http.MethodGet, p.getWarmupDetailURL(), "", "")
	if err == nil {
		if DebugLog {
			fmt.Printf("[Gying] 详情页状态码: %d\n", warmupStatus)
		}
		// 从详情页响应中收集cookies
		collectSetCookies(warmupHeaders, cookieMap)
		logSetCookiesForDebug(warmupHeaders, "[Gying]   详情页Cookie: %s=%s\n", 30)
	}

	// 构建cookie字符串
	cookieStr, err := p.exportCookies(scraper, p.getBaseURL())
	if err != nil {
		return nil, "", fmt.Errorf("导出cookie失败: %w", err)
	}
	cookieMap = parseCookieString(cookieStr)

	if DebugLog {
		fmt.Printf("[Gying] ✅ 登录成功！提取到 %d 个Cookie\n", len(cookieMap))
		fmt.Printf("[Gying] Cookie字符串长度: %d\n", len(cookieStr))
		for name, value := range cookieMap {
			displayValue := value
			if len(displayValue) > 30 {
				displayValue = displayValue[:30] + "..."
			}
			fmt.Printf("[Gying]   %s=%s (len:%d)\n", name, displayValue, len(value))
		}
		fmt.Printf("[Gying] ========== 登录完成 ==========\n")
	}

	return scraper, cookieStr, nil
}

// min 辅助函数
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ============ 重新登录逻辑 ============

// reloginUser 重新登录指定用户
func (p *GyingPlugin) reloginUser(user *User) error {
	if DebugLog {
		fmt.Printf("[Gying] 🔄 开始重新登录用户: %s\n", user.Username)
	}

	// 解密密码
	password, err := p.decryptPassword(user.EncryptedPassword)
	if err != nil {
		if DebugLog {
			fmt.Printf("[Gying] ❌ 解密密码失败: %v\n", err)
		}
		return fmt.Errorf("解密密码失败: %w", err)
	}

	// 执行登录
	scraper, cookie, err := p.doLogin(user.Username, password)
	if err != nil {
		if DebugLog {
			fmt.Printf("[Gying] ❌ 重新登录失败: %v\n", err)
		}
		return fmt.Errorf("重新登录失败: %w", err)
	}

	// 更新scraper实例
	p.scrapers.Store(user.Hash, scraper)

	// 更新用户信息
	user.Cookie = cookie
	user.LoginAt = time.Now()
	user.ExpireAt = time.Now().AddDate(0, 4, 0)
	user.Status = "active"

	if err := p.saveUser(user); err != nil {
		if DebugLog {
			fmt.Printf("[Gying] ⚠️  保存用户失败: %v\n", err)
		}
	}

	if DebugLog {
		fmt.Printf("[Gying] ✅ 用户 %s 重新登录成功\n", user.Username)
	}

	return nil
}

// ============ 搜索逻辑 ============

// executeSearchTasks 并发执行搜索任务
func (p *GyingPlugin) executeSearchTasks(users []*User, keyword string) []model.SearchResult {
	var allResults []model.SearchResult
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, user := range users {
		wg.Add(1)
		go func(u *User) {
			defer wg.Done()

			// 获取用户的scraper实例
			scraperVal, exists := p.scrapers.Load(u.Hash)
			var scraper *cloudscraper.Scraper

			if !exists {
				if DebugLog {
					fmt.Printf("[Gying] 用户 %s 没有scraper实例，尝试使用已保存的cookie创建\n", u.Username)
				}

				// 使用已保存的cookie创建scraper实例（关键！）
				newScraper, err := p.createScraperWithCookies(u.Cookie)
				if err != nil {
					if DebugLog {
						fmt.Printf("[Gying] 为用户 %s 创建scraper失败: %v\n", u.Username, err)
					}
					return
				}

				// 存储新创建的scraper实例
				p.scrapers.Store(u.Hash, newScraper)
				scraper = newScraper

				if DebugLog {
					fmt.Printf("[Gying] 已为用户 %s 恢复scraper实例（含cookie）\n", u.Username)
				}
			} else {
				var ok bool
				scraper, ok = scraperVal.(*cloudscraper.Scraper)
				if !ok || scraper == nil {
					if DebugLog {
						fmt.Printf("[Gying] 用户 %s scraper实例无效，跳过\n", u.Username)
					}
					return
				}
			}

			results, err := p.searchWithScraperWithRetry(keyword, scraper, u)
			if err != nil {
				if DebugLog {
					fmt.Printf("[Gying] 用户 %s 搜索失败（已重试）: %v\n", u.Username, err)
				}
				return
			}

			mu.Lock()
			allResults = append(allResults, results...)
			mu.Unlock()
		}(user)
	}

	wg.Wait()

	// 去重
	return p.deduplicateResults(allResults)
}

// searchWithScraperWithRetry 使用scraper搜索（带403自动重新登录重试）
func (p *GyingPlugin) searchWithScraperWithRetry(keyword string, scraper *cloudscraper.Scraper, user *User) ([]model.SearchResult, error) {
	results, err := p.searchWithScraper(keyword, scraper)
	if err == nil {
		if syncErr := p.syncUserCookiesFromScraper(user, scraper); syncErr != nil && DebugLog {
			fmt.Printf("[Gying] ⚠️  搜索后同步用户 %s Cookie失败: %v\n", user.Username, syncErr)
		}
	}

	// 检测是否为403错误
	if err != nil && strings.Contains(err.Error(), "403") {
		if DebugLog {
			fmt.Printf("[Gying] ⚠️  检测到403错误，尝试重新登录用户 %s\n", user.Username)
		}

		// 尝试重新登录
		if reloginErr := p.reloginUser(user); reloginErr != nil {
			if DebugLog {
				fmt.Printf("[Gying] ❌ 重新登录失败: %v\n", reloginErr)
			}
			return nil, fmt.Errorf("403错误且重新登录失败: %w", reloginErr)
		}

		// 获取新的scraper实例
		scraperVal, exists := p.scrapers.Load(user.Hash)
		if !exists {
			return nil, fmt.Errorf("重新登录后未找到scraper实例")
		}

		newScraper, ok := scraperVal.(*cloudscraper.Scraper)
		if !ok || newScraper == nil {
			return nil, fmt.Errorf("重新登录后scraper实例无效")
		}

		// 使用新scraper重试搜索
		if DebugLog {
			fmt.Printf("[Gying] 🔄 使用新登录状态重试搜索\n")
		}
		results, err = p.searchWithScraper(keyword, newScraper)
		if err != nil {
			return nil, fmt.Errorf("重新登录后搜索仍然失败: %w", err)
		}
		if syncErr := p.syncUserCookiesFromScraper(user, newScraper); syncErr != nil && DebugLog {
			fmt.Printf("[Gying] ⚠️  重登搜索后同步用户 %s Cookie失败: %v\n", user.Username, syncErr)
		}
	}

	return results, err
}

// searchWithScraper 使用scraper搜索
func (p *GyingPlugin) searchWithScraper(keyword string, scraper *cloudscraper.Scraper) ([]model.SearchResult, error) {
	if DebugLog {
		fmt.Printf("[Gying] ---------- searchWithScraper 开始 ----------\n")
		fmt.Printf("[Gying] 关键词: %s\n", keyword)
	}

	// 1. 使用cloudscraper请求搜索页面
	// searchURL := fmt.Sprintf("%s/s/1---1/%s", p.getBaseURL(), url.QueryEscape(keyword))
	searchURL := fmt.Sprintf("%s/search?q=%s&type=0&mode=2", p.getBaseURL(), url.QueryEscape(keyword))

	if DebugLog {
		fmt.Printf("[Gying] 搜索URL: %s\n", searchURL)
		fmt.Printf("[Gying] 使用cloudscraper发送请求\n")
	}

	body, statusCode, _, err := p.requestWithChallengeRetry(scraper, http.MethodGet, searchURL, "", "")
	if err != nil {
		if DebugLog {
			fmt.Printf("[Gying] 搜索请求失败: %v\n", err)
		}
		return nil, err
	}

	if DebugLog {
		fmt.Printf("[Gying] 搜索响应状态码: %d\n", statusCode)
	}

	if DebugLog {
		fmt.Printf("[Gying] 响应Body长度: %d 字节\n", len(body))
		if len(body) > 0 {
			// 打印前500字符
			preview := string(body)
			if len(preview) > 500 {
				preview = preview[:500] + "..."
			}
			fmt.Printf("[Gying] 响应预览: %s\n", preview)
		}
	}

	// 检查403错误
	if statusCode == http.StatusForbidden {
		if DebugLog {
			fmt.Printf("[Gying] ❌ 收到403 Forbidden - Cookie可能已过期或被网站拒绝\n")
			if len(body) > 0 {
				preview := string(body)
				if len(preview) > 300 {
					preview = preview[:300] + "..."
				}
				fmt.Printf("[Gying] 403响应内容: %s\n", preview)
			}
		}
		return nil, fmt.Errorf("HTTP 403 Forbidden - 可能需要重新登录")
	}

	// 2. 提取 _obj.search JSON
	matches := searchDataPattern.FindSubmatch(body)

	if DebugLog {
		fmt.Printf("[Gying] 正则匹配结果: 找到 %d 个匹配\n", len(matches))
	}

	if isLoginShell(body) {
		return nil, fmt.Errorf("HTTP 403 Forbidden - 需要重新登录")
	}

	if len(matches) < 2 {
		if DebugLog {
			fmt.Printf("[Gying] ❌ 未找到 _obj.search JSON数据\n")

			// 尝试查找是否有其他模式
			if strings.Contains(string(body), "_obj.search") {
				fmt.Printf("[Gying] 但是Body中包含 '_obj.search' 字符串\n")
			} else {
				fmt.Printf("[Gying] Body中不包含 '_obj.search' 字符串\n")
			}
		}
		return nil, fmt.Errorf("未找到搜索结果数据")
	}

	if DebugLog {
		jsonStr := string(matches[1])
		if len(jsonStr) > 200 {
			jsonStr = jsonStr[:200] + "..."
		}
		fmt.Printf("[Gying] 提取的JSON数据: %s\n", jsonStr)
	}

	var searchData SearchData
	if err := json.Unmarshal(matches[1], &searchData); err != nil {
		if DebugLog {
			fmt.Printf("[Gying] JSON解析失败: %v\n", err)
			fmt.Printf("[Gying] 原始JSON: %s\n", string(matches[1]))
		}
		return nil, fmt.Errorf("解析搜索数据失败: %w", err)
	}

	if DebugLog {
		fmt.Printf("[Gying] 搜索数据解析成功:\n")
		fmt.Printf("[Gying]   - 关键词: %s\n", searchData.Q)
		fmt.Printf("[Gying]   - 结果数量字符串: %s\n", searchData.N)
		fmt.Printf("[Gying]   - 资源ID数组长度: %d\n", len(searchData.L.I))
		fmt.Printf("[Gying]   - 标题数组长度: %d\n", len(searchData.L.Title))
		if len(searchData.L.I) > 0 {
			fmt.Printf("[Gying]   - 前3个资源ID: %v\n", searchData.L.I[:min(3, len(searchData.L.I))])
			fmt.Printf("[Gying]   - 前3个标题: %v\n", searchData.L.Title[:min(3, len(searchData.L.Title))])
		}
	}

	// 3. 刷新防爬cookies（关键！访问详情页触发vrg_sc、vrg_go等防爬cookies）
	if DebugLog {
		fmt.Printf("[Gying] 刷新防爬cookies...\n")
	}
	_, refreshStatus, _, err := p.requestWithChallengeRetry(scraper, http.MethodGet, p.getWarmupDetailURL(), "", "")
	if err == nil {
		if DebugLog {
			fmt.Printf("[Gying] 防爬cookies刷新成功 (状态码: %d)\n", refreshStatus)
		}
	}

	// 4. 并发请求详情接口
	results, err := p.fetchAllDetails(&searchData, scraper, keyword)
	if err != nil {
		if DebugLog {
			fmt.Printf("[Gying] fetchAllDetails 失败: %v\n", err)
			fmt.Printf("[Gying] ---------- searchWithScraper 结束 ----------\n")
		}
		return nil, err
	}

	if DebugLog {
		fmt.Printf("[Gying] fetchAllDetails 返回 %d 条结果\n", len(results))
		fmt.Printf("[Gying] ---------- searchWithScraper 结束 ----------\n")
	}

	return results, nil
}

// fetchAllDetails 并发获取所有详情
func (p *GyingPlugin) fetchAllDetails(searchData *SearchData, scraper *cloudscraper.Scraper, keyword string) ([]model.SearchResult, error) {
	if DebugLog {
		fmt.Printf("[Gying] >>> fetchAllDetails 开始\n")
		fmt.Printf("[Gying] 需要获取 %d 个详情，关键词: %s\n", len(searchData.L.I), keyword)
	}

	var results []model.SearchResult
	var mu sync.Mutex
	var wg sync.WaitGroup

	semaphore := make(chan struct{}, MaxConcurrentDetails)
	errChan := make(chan error, 1) // 用于接收403错误

	successCount := 0
	failCount := 0
	has403 := false

	// 将关键词转为小写，用于不区分大小写的匹配
	keywordLower := strings.ToLower(keyword)

	for i := 0; i < len(searchData.L.I); i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()

			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			// 检查是否已经遇到403错误
			mu.Lock()
			if has403 {
				mu.Unlock()
				return
			}
			mu.Unlock()

			// 检查标题是否包含搜索关键词
			if index >= len(searchData.L.Title) {
				if DebugLog {
					fmt.Printf("[Gying]   [%d/%d] ⏭️  跳过: 索引超出标题数组范围\n",
						index+1, len(searchData.L.I))
				}
				return
			}

			title := searchData.L.Title[index]
			titleLower := strings.ToLower(title)
			if !strings.Contains(titleLower, keywordLower) {
				if DebugLog {
					fmt.Printf("[Gying]   [%d/%d] ⏭️  跳过: 标题不包含关键词 '%s' (标题: %s)\n",
						index+1, len(searchData.L.I), keyword, title)
				}
				return
			}

			if DebugLog {
				fmt.Printf("[Gying]   [%d/%d] 获取详情: ID=%s, Type=%s, 标题=%s\n",
					index+1, len(searchData.L.I), searchData.L.I[index], searchData.L.D[index], title)
			}

			detail, err := p.fetchDetail(searchData.L.I[index], searchData.L.D[index], scraper)
			if err != nil {
				if DebugLog {
					fmt.Printf("[Gying]   [%d/%d] ❌ 获取详情失败: %v\n", index+1, len(searchData.L.I), err)
				}

				// 检查是否是403错误
				if strings.Contains(err.Error(), "403") {
					mu.Lock()
					if !has403 {
						has403 = true
						select {
						case errChan <- err:
						default:
						}
					}
					mu.Unlock()
				}

				mu.Lock()
				failCount++
				mu.Unlock()
				return
			}

			result := p.buildResult(detail, searchData, index)
			if result.Title != "" && len(result.Links) > 0 {
				if DebugLog {
					fmt.Printf("[Gying]   [%d/%d] ✅ 成功: %s (%d个链接)\n",
						index+1, len(searchData.L.I), result.Title, len(result.Links))
				}
				mu.Lock()
				results = append(results, result)
				successCount++
				mu.Unlock()
			} else {
				if DebugLog {
					fmt.Printf("[Gying]   [%d/%d] ⚠️  跳过: 标题或链接为空 (标题:%s, 链接数:%d)\n",
						index+1, len(searchData.L.I), result.Title, len(result.Links))
				}
			}
		}(i)
	}

	wg.Wait()

	// 检查是否有403错误
	select {
	case err := <-errChan:
		if DebugLog {
			fmt.Printf("[Gying] <<< fetchAllDetails 检测到403错误，需要重新登录\n")
		}
		return nil, err
	default:
	}

	if DebugLog {
		fmt.Printf("[Gying] <<< fetchAllDetails 完成: 成功=%d, 失败=%d, 总计=%d\n",
			successCount, failCount, len(searchData.L.I))
	}

	return results, nil
}

// fetchDetail 获取详情
func (p *GyingPlugin) fetchDetail(resourceID, resourceType string, scraper *cloudscraper.Scraper) (*DetailData, error) {
	detailURL := fmt.Sprintf("%s/res/downurl/%s/%s", p.getBaseURL(), resourceType, resourceID)

	if DebugLog {
		fmt.Printf("[Gying]     fetchDetail: %s\n", detailURL)
	}

	// 使用cloudscraper发送请求（自动管理Cookie和绕过反爬虫）
	body, statusCode, _, err := p.requestWithChallengeRetry(scraper, http.MethodGet, detailURL, "", "")
	if err != nil {
		if DebugLog {
			fmt.Printf("[Gying]     请求失败: %v\n", err)
		}
		return nil, err
	}

	if DebugLog {
		fmt.Printf("[Gying]     响应状态码: %d\n", statusCode)
	}

	// 检查403错误
	if statusCode == http.StatusForbidden {
		if DebugLog {
			fmt.Printf("[Gying]     ❌ 详情接口返回403 - Cookie可能已过期\n")
		}
		return nil, fmt.Errorf("HTTP 403 Forbidden")
	}

	if statusCode != http.StatusOK {
		if DebugLog {
			fmt.Printf("[Gying]     ❌ HTTP错误: %d\n", statusCode)
		}
		return nil, fmt.Errorf("HTTP %d", statusCode)
	}

	if DebugLog {
		fmt.Printf("[Gying]     响应长度: %d 字节\n", len(body))
	}
	if isLoginShell(body) {
		return nil, fmt.Errorf("HTTP 403 Forbidden")
	}

	var detail DetailData
	if err := json.Unmarshal(body, &detail); err != nil {
		if DebugLog {
			fmt.Printf("[Gying]     JSON解析失败: %v\n", err)
			// 打印前200字符
			preview := string(body)
			if len(preview) > 200 {
				preview = preview[:200] + "..."
			}
			fmt.Printf("[Gying]     响应内容: %s\n", preview)
		}
		return nil, err
	}

	if DebugLog {
		fmt.Printf("[Gying]     详情Code: %d, 网盘链接数: %d\n", detail.Code, len(detail.Panlist.URL))
	}

	// 检查JSON响应中的code字段（关键！）
	if detail.Code == 403 {
		if DebugLog {
			fmt.Printf("[Gying]     ❌ 详情接口返回Code=403 - 登录状态可能已失效\n")
		}
		return nil, fmt.Errorf("详情接口返回 code=403，登录状态可能已失效")
	}

	return &detail, nil
}

// buildResult 构建SearchResult
func (p *GyingPlugin) buildResult(detail *DetailData, searchData *SearchData, index int) model.SearchResult {
	if index >= len(searchData.L.Title) {
		return model.SearchResult{}
	}

	title := searchData.L.Title[index]
	resourceType := searchData.L.D[index]
	resourceID := searchData.L.I[index]

	// 获取年份并拼接到标题后面
	var year int
	if index < len(searchData.L.Year) && searchData.L.Year[index] > 0 {
		year = searchData.L.Year[index]
		// 拼接年份到标题：遮天（2023）
		title = fmt.Sprintf("%s（%d）", title, year)
	}

	// 构建描述
	var contentParts []string
	if index < len(searchData.L.Info) && searchData.L.Info[index] != "" {
		contentParts = append(contentParts, searchData.L.Info[index])
	}
	if index < len(searchData.L.Daoyan) && searchData.L.Daoyan[index] != "" {
		contentParts = append(contentParts, fmt.Sprintf("导演: %s", searchData.L.Daoyan[index]))
	}
	if index < len(searchData.L.Zhuyan) && searchData.L.Zhuyan[index] != "" {
		contentParts = append(contentParts, fmt.Sprintf("主演: %s", searchData.L.Zhuyan[index]))
	}

	// 同时提取网盘和磁力链接
	links := p.extractLinks(detail, title)

	// 构建标签（保留年份标签，提供额外的过滤维度）
	var tags []string
	if year > 0 {
		tags = append(tags, fmt.Sprintf("%d", year))
	}

	// 从网盘时间数组中选择最新的时间（最小的相对时间值）
	// 检查 detail 是否为 nil
	var datetime time.Time
	if detail == nil {
		if DebugLog {
			fmt.Printf("[Gying] buildResult: detail为nil，使用当前时间\n")
		}
		datetime = time.Now()
	} else {
		detailTimes := p.collectDetailTimes(detail)
		datetime = p.parseUpdateTime(detailTimes)
		if DebugLog {
			fmt.Printf("[Gying] buildResult时间解析: 时间数组长度=%d, 解析后时间=%v\n",
				len(detailTimes), datetime.Format("2006-01-02 15:04:05"))
			if len(detailTimes) > 0 {
				fmt.Printf("[Gying]   前3个时间字符串: %v\n", detailTimes[:min(3, len(detailTimes))])
			}
		}
	}

	return model.SearchResult{
		UniqueID: fmt.Sprintf("gying-%s-%s", resourceType, resourceID),
		Title:    title,
		Content:  strings.Join(contentParts, " | "),
		Links:    links,
		Tags:     tags,
		Channel:  "", // 插件搜索结果Channel为空
		Datetime: datetime,
	}
}

// parseUpdateTime 解析网盘更新时间数组，返回最新的更新时间
// 时间字符串格式：["今天", "昨天", "2天前", "1月前", "1年前"] 等
func (p *GyingPlugin) parseUpdateTime(timeStrs []string) time.Time {
	// 处理 nil slice 的情况
	if timeStrs == nil || len(timeStrs) == 0 {
		if DebugLog {
			fmt.Printf("[Gying] parseUpdateTime: 时间数组为空或nil，返回当前时间\n")
		}
		// 如果没有时间信息，返回当前时间
		return time.Now()
	}

	now := time.Now()
	var latestTime *time.Time

	if DebugLog {
		fmt.Printf("[Gying] parseUpdateTime: 开始解析 %d 个时间字符串\n", len(timeStrs))
	}

	// 遍历所有时间字符串，找到最新的（最接近当前时间的）那个
	for i, timeStr := range timeStrs {
		if timeStr == "" {
			continue
		}

		parsedTime := p.parseRelativeTime(timeStr, now)
		if parsedTime != nil {
			if DebugLog && i < 5 { // 只打印前5个，避免日志过多
				fmt.Printf("[Gying]   [%d] '%s' -> %v\n", i, timeStr, parsedTime.Format("2006-01-02 15:04:05"))
			}
			// 找到最接近当前时间的（最新的）
			if latestTime == nil || parsedTime.After(*latestTime) {
				latestTime = parsedTime
			}
		} else {
			if DebugLog && i < 5 {
				fmt.Printf("[Gying]   [%d] '%s' -> 解析失败\n", i, timeStr)
			}
		}
	}

	// 如果解析失败，返回当前时间
	if latestTime == nil {
		if DebugLog {
			fmt.Printf("[Gying] parseUpdateTime: 所有时间解析失败，返回当前时间\n")
			// 输出前几个时间字符串用于调试
			if len(timeStrs) > 0 {
				fmt.Printf("[Gying]   前3个时间字符串: %v\n", timeStrs[:min(3, len(timeStrs))])
			}
		}
		return time.Now()
	}

	if DebugLog {
		fmt.Printf("[Gying] parseUpdateTime: 最终选择时间 %v\n", latestTime.Format("2006-01-02 15:04:05"))
	}
	return *latestTime
}

// parseRelativeTime 解析单个相对时间字符串，返回对应的time.Time
// 支持格式：今天、昨天、N天前、N月前、N年前
func (p *GyingPlugin) parseRelativeTime(timeStr string, baseTime time.Time) *time.Time {
	timeStr = strings.TrimSpace(timeStr)
	if timeStr == "" {
		return nil
	}

	switch timeStr {
	case "今天":
		t := baseTime.Truncate(24 * time.Hour)
		return &t
	case "昨天":
		t := baseTime.AddDate(0, 0, -1).Truncate(24 * time.Hour)
		return &t
	default:
		// 解析 "N天前"、"N月前"、"N年前" 格式
		if strings.HasSuffix(timeStr, "天前") {
			daysStr := strings.TrimSuffix(timeStr, "天前")
			days, err := strconv.Atoi(daysStr)
			if err == nil && days >= 0 {
				t := baseTime.AddDate(0, 0, -days).Truncate(24 * time.Hour)
				return &t
			}
		} else if strings.HasSuffix(timeStr, "月前") {
			monthsStr := strings.TrimSuffix(timeStr, "月前")
			months, err := strconv.Atoi(monthsStr)
			if err == nil && months >= 0 {
				t := baseTime.AddDate(0, -months, 0).Truncate(24 * time.Hour)
				return &t
			}
		} else if strings.HasSuffix(timeStr, "年前") {
			yearsStr := strings.TrimSuffix(timeStr, "年前")
			years, err := strconv.Atoi(yearsStr)
			if err == nil && years >= 0 {
				t := baseTime.AddDate(-years, 0, 0).Truncate(24 * time.Hour)
				return &t
			}
		}
	}

	// 无法解析，返回nil
	return nil
}

// collectDetailTimes 汇总详情里的所有时间字段，优先用于结果时间判断
func (p *GyingPlugin) collectDetailTimes(detail *DetailData) []string {
	if detail == nil {
		return nil
	}

	times := make([]string, 0, len(detail.Panlist.Time)+len(detail.Downlist.List.N))
	times = append(times, detail.Panlist.Time...)
	times = append(times, detail.Downlist.List.N...)
	return times
}

// extractLinks 提取详情中的所有链接
func (p *GyingPlugin) extractLinks(detail *DetailData, resultTitle string) []model.Link {
	if detail == nil {
		return nil
	}

	now := time.Now()
	seen := make(map[string]struct{})
	links := make([]model.Link, 0, len(detail.Panlist.URL)+len(detail.Downlist.List.M))

	links = append(links, p.extractPanLinks(detail, resultTitle, now, seen)...)
	links = append(links, p.extractMagnetLinks(detail, resultTitle, now, seen)...)

	return links
}

// extractPanLinks 提取网盘链接
func (p *GyingPlugin) extractPanLinks(detail *DetailData, resultTitle string, now time.Time, seen map[string]struct{}) []model.Link {
	links := make([]model.Link, 0, len(detail.Panlist.URL))

	for i := 0; i < len(detail.Panlist.URL); i++ {
		rawURL := p.safeString(detail.Panlist.URL, i)
		typeCode := p.safeInt(detail.Panlist.Type, i, -1)
		typeName := p.getPanTypeName(detail, typeCode)

		linkURL := p.normalizePanURL(rawURL, typeCode, typeName)
		linkType := p.determineLinkType(linkURL, typeCode, typeName)
		if linkURL == "" || linkType == "others" {
			continue
		}

		seenKey := linkType + ":" + strings.ToLower(linkURL)
		if _, exists := seen[seenKey]; exists {
			continue
		}
		seen[seenKey] = struct{}{}

		linkTime := p.parseLinkTime(p.safeString(detail.Panlist.Time, i), now)
		resourceName := p.safeString(detail.Panlist.Name, i)
		password := p.extractPassword(rawURL, p.safeString(detail.Panlist.P, i))

		links = append(links, model.Link{
			Type:      linkType,
			URL:       linkURL,
			Password:  password,
			Datetime:  linkTime,
			WorkTitle: p.buildLinkWorkTitle(resultTitle, resourceName),
		})
	}

	return links
}

// extractMagnetLinks 从 downlist 手动拼接磁力链接
func (p *GyingPlugin) extractMagnetLinks(detail *DetailData, resultTitle string, now time.Time, seen map[string]struct{}) []model.Link {
	hashes := detail.Downlist.List.M
	if len(hashes) == 0 {
		return nil
	}

	links := make([]model.Link, 0, len(hashes))
	for i := 0; i < len(hashes); i++ {
		infoHash := strings.ToLower(strings.TrimSpace(hashes[i]))
		if !magnetHashRegex.MatchString(infoHash) {
			continue
		}

		seenKey := "magnet:" + infoHash
		if _, exists := seen[seenKey]; exists {
			continue
		}
		seen[seenKey] = struct{}{}

		resourceName := p.safeString(detail.Downlist.List.T, i)
		if resourceName == "" {
			resourceName = p.safeString(detail.Downlist.List.S, i)
		}

		magnetURL := p.buildMagnetURL(infoHash, resourceName)
		if magnetURL == "" {
			continue
		}

		links = append(links, model.Link{
			Type:      "magnet",
			URL:       magnetURL,
			Password:  "",
			Datetime:  p.parseLinkTime(p.safeString(detail.Downlist.List.N, i), now),
			WorkTitle: p.buildLinkWorkTitle(resultTitle, resourceName),
		})
	}

	return links
}

func (p *GyingPlugin) getPanTypeName(detail *DetailData, typeCode int) string {
	if detail == nil || typeCode < 0 || typeCode >= len(detail.Panlist.TName) {
		return ""
	}
	return strings.TrimSpace(detail.Panlist.TName[typeCode])
}

// determineLinkType 识别链接类型，先看URL，再看类型编码和名称兜底
func (p *GyingPlugin) determineLinkType(linkURL string, typeCode int, typeName string) string {
	lowerURL := strings.ToLower(strings.TrimSpace(linkURL))
	lowerTypeName := strings.ToLower(strings.TrimSpace(typeName))

	switch {
	case strings.HasPrefix(lowerURL, "magnet:?xt=urn:btih:"):
		return "magnet"
	case strings.Contains(lowerURL, "pan.quark.cn"):
		return "quark"
	case strings.Contains(lowerURL, "drive.uc.cn"):
		return "uc"
	case strings.Contains(lowerURL, "pan.baidu.com"):
		return "baidu"
	case strings.Contains(lowerURL, "aliyundrive.com") || strings.Contains(lowerURL, "alipan.com"):
		return "aliyun"
	case strings.Contains(lowerURL, "pan.xunlei.com"):
		return "xunlei"
	case strings.Contains(lowerURL, "cloud.189.cn") || strings.Contains(lowerURL, "content.21cn.com") || strings.Contains(lowerURL, "tianyi.cloud"):
		return "tianyi"
	case strings.Contains(lowerURL, "yun.139.com") || strings.Contains(lowerURL, "caiyun.139.com") || strings.Contains(lowerURL, "feixin.10086.cn"):
		return "mobile"
	case strings.Contains(lowerURL, "115.com") || strings.Contains(lowerURL, "115cdn.com") || strings.Contains(lowerURL, "anxia.com"):
		return "115"
	case strings.Contains(lowerURL, "123684.com") || strings.Contains(lowerURL, "123685.com") ||
		strings.Contains(lowerURL, "123865.com") || strings.Contains(lowerURL, "123912.com") ||
		strings.Contains(lowerURL, "123pan.com") || strings.Contains(lowerURL, "123pan.cn") ||
		strings.Contains(lowerURL, "123592.com"):
		return "123"
	}

	if mappedType, ok := gyingPanTypeMap[typeCode]; ok {
		return mappedType
	}

	switch {
	case strings.Contains(lowerTypeName, "天翼"):
		return "tianyi"
	case strings.Contains(lowerTypeName, "移动") || strings.Contains(lowerTypeName, "彩云"):
		return "mobile"
	case strings.Contains(lowerTypeName, "百度"):
		return "baidu"
	case strings.Contains(lowerTypeName, "夸克"):
		return "quark"
	case strings.Contains(lowerTypeName, "迅雷"):
		return "xunlei"
	case strings.Contains(lowerTypeName, "阿里"):
		return "aliyun"
	case strings.Contains(lowerTypeName, "115"):
		return "115"
	case strings.Contains(lowerTypeName, "123"):
		return "123"
	case strings.Contains(lowerTypeName, "uc"):
		return "uc"
	default:
		return "others"
	}
}

func (p *GyingPlugin) normalizePanURL(rawURL string, typeCode int, typeName string) string {
	rawURL = strings.TrimSpace(rawURL)
	rawURL = accessCodeBlockRegex.ReplaceAllString(rawURL, "")
	rawURL = strings.TrimSpace(rawURL)

	linkType := p.determineLinkType(rawURL, typeCode, typeName)

	switch linkType {
	case "baidu":
		return baiduLinkRegex.FindString(rawURL)
	case "quark":
		return quarkLinkRegex.FindString(rawURL)
	case "aliyun":
		return aliyunLinkRegex.FindString(rawURL)
	case "xunlei":
		return xunleiLinkRegex.FindString(rawURL)
	case "tianyi":
		if code := p.extractTianyiShareCode(rawURL); code != "" {
			return "https://cloud.189.cn/t/" + code
		}
		if match := tianyiLinkRegex.FindString(rawURL); match != "" {
			return match
		}
		return tianyiCloudRegex.FindString(rawURL)
	case "mobile":
		if match := mobileYunLinkRegex.FindString(rawURL); match != "" {
			return match
		}
		if match := mobileCaiyunLinkRegex.FindString(rawURL); match != "" {
			return match
		}
		return mobileFeixinLinkRegex.FindString(rawURL)
	case "115":
		return link115Regex.FindString(rawURL)
	case "123":
		return link123Regex.FindString(rawURL)
	case "uc":
		return ucLinkRegex.FindString(rawURL)
	default:
		return ""
	}
}

func (p *GyingPlugin) extractTianyiShareCode(rawURL string) string {
	matches := tianyiShareCodeRegex.FindStringSubmatch(rawURL)
	if len(matches) < 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

func (p *GyingPlugin) extractPassword(rawText, fallback string) string {
	if password := p.extractPasswordFromText(rawText); password != "" {
		return password
	}
	return p.normalizePassword(fallback)
}

// extractPasswordFromText 从URL或附带说明中提取提取码
func (p *GyingPlugin) extractPasswordFromText(text string) string {
	for _, pattern := range inlinePasswordPatterns {
		if matches := pattern.FindStringSubmatch(text); len(matches) > 1 {
			return p.normalizePassword(matches[1])
		}
	}
	return ""
}

func (p *GyingPlugin) normalizePassword(raw string) string {
	password := strings.TrimSpace(raw)
	if password == "" {
		return ""
	}

	password = strings.Trim(password, ".。!！,，;；:：#*· ")
	lower := strings.ToLower(password)
	if lower == "无提取码" || strings.Contains(lower, "无密码") || strings.Contains(password, "无需") {
		return ""
	}

	if exactPasswordRegex.MatchString(password) {
		return password
	}

	for _, pattern := range inlinePasswordPatterns {
		if matches := pattern.FindStringSubmatch(password); len(matches) > 1 {
			candidate := strings.TrimSpace(matches[1])
			if exactPasswordRegex.MatchString(candidate) {
				return candidate
			}
		}
	}

	return ""
}

func (p *GyingPlugin) buildMagnetURL(infoHash, resourceName string) string {
	infoHash = strings.ToLower(strings.TrimSpace(infoHash))
	if !magnetHashRegex.MatchString(infoHash) {
		return ""
	}

	magnetURL := "magnet:?xt=urn:btih:" + infoHash
	resourceName = strings.TrimSpace(resourceName)
	if resourceName != "" {
		magnetURL += "&dn=" + url.QueryEscape(resourceName)
	}
	return magnetURL
}

func (p *GyingPlugin) parseLinkTime(timeStr string, baseTime time.Time) time.Time {
	if parsedTime := p.parseRelativeTime(timeStr, baseTime); parsedTime != nil {
		return *parsedTime
	}
	return time.Time{}
}

func (p *GyingPlugin) buildLinkWorkTitle(resultTitle, resourceName string) string {
	resultTitle = strings.TrimSpace(resultTitle)
	resourceName = strings.TrimSpace(resourceName)

	if resourceName == "" {
		return resultTitle
	}

	resultKey := p.normalizeTitleForCompare(resultTitle)
	resourceKey := p.normalizeTitleForCompare(resourceName)
	if resultKey != "" && strings.Contains(resourceKey, resultKey) {
		return resourceName
	}

	if resultTitle == "" {
		return resourceName
	}
	return resultTitle + " - " + resourceName
}

func (p *GyingPlugin) normalizeTitleForCompare(title string) string {
	title = yearSuffixRegex.ReplaceAllString(title, "")
	title = strings.ToLower(strings.TrimSpace(title))

	replacer := strings.NewReplacer(
		" ", "",
		"-", "",
		"_", "",
		".", "",
		"：", "",
		":", "",
		"（", "",
		"）", "",
		"(", "",
		")", "",
		"【", "",
		"】", "",
		"[", "",
		"]", "",
		"/", "",
	)
	return replacer.Replace(title)
}

func (p *GyingPlugin) safeString(items []string, index int) string {
	if index < 0 || index >= len(items) {
		return ""
	}
	return strings.TrimSpace(items[index])
}

func (p *GyingPlugin) safeInt(items []int, index int, fallback int) int {
	if index < 0 || index >= len(items) {
		return fallback
	}
	return items[index]
}

// deduplicateResults 去重
func (p *GyingPlugin) deduplicateResults(results []model.SearchResult) []model.SearchResult {
	seen := make(map[string]bool)
	var deduplicated []model.SearchResult

	for _, result := range results {
		if !seen[result.UniqueID] {
			seen[result.UniqueID] = true
			deduplicated = append(deduplicated, result)
		}
	}

	return deduplicated
}

// ============ 工具函数 ============

// generateHash 生成hash
func (p *GyingPlugin) generateHash(username string) string {
	salt := os.Getenv("GYING_HASH_SALT")
	if salt == "" {
		salt = "pansou_gying_secret_2025"
	}
	data := username + salt
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// isHexString 判断是否为十六进制
func (p *GyingPlugin) isHexString(s string) bool {
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

// ============ Cookie加密（可选） ============

func getEncryptionKey() []byte {
	key := os.Getenv("GYING_ENCRYPTION_KEY")
	if key == "" {
		key = "default-32-byte-key-change-me!"
	}
	return []byte(key)[:32]
}

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

// ============ Session保活 ============

// startSessionKeepAlive 启动session保活任务
func (p *GyingPlugin) startSessionKeepAlive() {
	// 首次启动后延迟3分钟再开始（避免启动时过多请求）
	time.Sleep(3 * time.Minute)

	// 立即执行一次保活
	p.keepAllSessionsAlive()

	// 每3分钟执行一次保活
	ticker := time.NewTicker(3 * time.Minute)
	for range ticker.C {
		p.keepAllSessionsAlive()
	}
}

// keepAllSessionsAlive 保持所有用户的session活跃
func (p *GyingPlugin) keepAllSessionsAlive() {
	count := 0

	p.users.Range(func(key, value interface{}) bool {
		user := value.(*User)

		// 只为active状态的用户保活
		if user.Status != "active" {
			return true
		}

		// 获取scraper实例
		scraperVal, exists := p.scrapers.Load(user.Hash)
		if !exists {
			return true
		}

		scraper, ok := scraperVal.(*cloudscraper.Scraper)
		if !ok || scraper == nil {
			return true
		}

		// 访问首页保持session活跃
		go func(s *cloudscraper.Scraper, username, homeURL string) {
			resp, err := s.Get(homeURL)
			if err == nil && resp != nil {
				resp.Body.Close()
				if DebugLog {
					fmt.Printf("[Gying] 💓 Session保活成功: %s (状态码: %d)\n", username, resp.StatusCode)
				}
			}
		}(scraper, user.Username, p.getBaseURL()+"/")

		count++
		return true
	})

	if DebugLog && count > 0 {
		fmt.Printf("[Gying] 💓 已为 %d 个用户执行session保活\n", count)
	}
}

// ============ 定期清理 ============

func (p *GyingPlugin) startCleanupTask() {
	ticker := time.NewTicker(24 * time.Hour)
	for range ticker.C {
		deleted := p.cleanupExpiredUsers()
		marked := p.markInactiveUsers()

		if deleted > 0 || marked > 0 {
			fmt.Printf("[Gying] 清理任务完成: 删除 %d 个过期用户, 标记 %d 个不活跃用户\n", deleted, marked)
		}
	}
}

func (p *GyingPlugin) cleanupExpiredUsers() int {
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

func (p *GyingPlugin) markInactiveUsers() int {
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
