# PanSou 网盘搜索API

PanSou是一个高性能的网盘资源搜索API服务，支持TG搜索和自定义插件搜索。系统设计以性能和可扩展性为核心，支持并发搜索、结果智能排序和网盘类型分类。

## 特性

- **高性能搜索**：并发搜索多个Telegram频道，显著提升搜索速度；工作池设计，高效管理并发任务
- **网盘类型分类**：自动识别多种网盘链接，按类型归类展示
- **智能排序**：基于时间和关键词权重的多级排序策略
- **异步插件系统**：支持通过插件扩展搜索来源，已内置多个网盘搜索插件，详情参考[插件开发指南.md](docs/插件开发指南.md)；支持"尽快响应，持续处理"的异步搜索模式，解决了某些搜索源响应时间长的问题
  - **双级超时控制**：短超时(4秒)确保快速响应，长超时(30秒)允许完整处理
  - **持久化缓存**：缓存自动保存到磁盘，系统重启后自动恢复
  - **优雅关闭**：在程序退出前保存缓存，确保数据不丢失
  - **增量更新**：智能合并新旧结果，保留有价值的数据
  - **主动更新**：异步插件在缓存异步更新后会主动更新主缓存(内存+磁盘)，使用户在不强制刷新的情况下也能获取最新数据
- **插件扩展参数**：通过ext参数向插件传递自定义搜索参数，如英文标题、全量搜索标志等，提高搜索灵活性和精确度
- **二级缓存**：内存+分片磁盘缓存机制，大幅提升重复查询速度和并发性能  
  - **分片磁盘缓存**：将缓存数据分散到多个子目录，减少锁竞争，通过哈希算法将缓存键均匀分布到不同分片，提高高并发场景下的性能
  - **序列化器接口**：Gob序列化提供更高性能和更小的结果大小
  - **分离的缓存键**：TG搜索和插件搜索使用独立的缓存键，实现独立更新，互不影响，提高缓存命中率和更新效率
  - **优化的缓存读取策略**：优先使用内存缓存，其次从磁盘读取缓存数据
- **优化的HTTP服务器配置**：
  - **自动计算的超时设置**：根据系统配置和异步插件需求自动调整读取超时、写入超时和空闲超时
  - **连接数限制**：根据CPU核心数自动计算最大并发连接数，防止资源耗尽
  - **高性能连接管理**：优化的连接复用和释放策略，提高高并发场景下的性能

## 支持的网盘类型

- 百度网盘 (`pan.baidu.com`)
- 阿里云盘 (`aliyundrive.com`, `alipan.com`)
- 夸克网盘 (`pan.quark.cn`)
- 天翼云盘 (`cloud.189.cn`)
- UC网盘 (`drive.uc.cn`)
- 移动云盘 (`caiyun.139.com`)
- 115网盘 (`115.com`, `115cdn.com`, `anxia.com`)
- PikPak (`mypikpak.com`)
- 迅雷网盘 (`pan.xunlei.com`)
- 123网盘 (`123684.com`, `123685.com`, `123912.com`, `123pan.com`, `123pan.cn`, `123592.com`)
- 磁力链接 (`magnet:?xt=urn:btih:`)
- 电驴链接 (`ed2k://`)

## API文档

### 搜索API

搜索网盘资源。

**接口地址**：`/api/search`  
**请求方法**：`POST` 或 `GET`  
**Content-Type**：`application/json`（POST方法）

**POST请求参数**：

| 参数名 | 类型 | 必填 | 描述 |
|--------|------|------|------|
| kw | string | 是 | 搜索关键词 |
| channels | string[] | 否 | 搜索的频道列表，不提供则使用默认配置 |
| conc | number | 否 | 并发搜索数量，不提供则自动设置为频道数+插件数+10 |
| refresh | boolean | 否 | 强制刷新，不使用缓存，便于调试和获取最新数据 |
| res | string | 否 | 结果类型：all(返回所有结果)、results(仅返回results)、merge(仅返回merged_by_type)，默认为merge |
| src | string | 否 | 数据来源类型：all(默认，全部来源)、tg(仅Telegram)、plugin(仅插件) |
| plugins | string[] | 否 | 指定搜索的插件列表，不指定则搜索全部插件 |
| ext | object | 否 | 扩展参数，用于传递给插件的自定义参数，如{"title_en":"English Title", "is_all":true} |

**GET请求参数**：

| 参数名 | 类型 | 必填 | 描述 |
|--------|------|------|------|
| kw | string | 是 | 搜索关键词 |
| channels | string | 否 | 搜索的频道列表，使用英文逗号分隔多个频道，不提供则使用默认配置 |
| conc | number | 否 | 并发搜索数量，不提供则自动设置为频道数+插件数+10 |
| refresh | boolean | 否 | 强制刷新，设置为"true"表示不使用缓存 |
| res | string | 否 | 结果类型：all(返回所有结果)、results(仅返回results)、merge(仅返回merged_by_type)，默认为merge |
| src | string | 否 | 数据来源类型：all(默认，全部来源)、tg(仅Telegram)、plugin(仅插件) |
| plugins | string | 否 | 指定搜索的插件列表，使用英文逗号分隔多个插件名，不指定则搜索全部插件 |
| ext | string | 否 | JSON格式的扩展参数，用于传递给插件的自定义参数，如{"title_en":"English Title", "is_all":true} |

**POST请求示例**：

```json
{
  "kw": "速度与激情",
  "channels": ["tgsearchers2", "xxx"],
  "conc": 2,
  "refresh": true,
  "res": "merge",
  "src": "all",
  "plugins": ["jikepan"],
  "ext": {
    "title_en": "Fast and Furious",
    "is_all": true
  }
}
```

**GET请求示例**：

```
GET /api/search?kw=速度与激情&channels=tgsearchers2,xxx&conc=2&refresh=true&res=merge&src=tg&ext={"title_en":"Fast and Furious","is_all":true}
```

**成功响应**：

```json
{
  "total": 15,
  "results": [
    {
      "message_id": "12345",
      "unique_id": "channel-12345",
      "channel": "tgsearchers2",
      "datetime": "2023-06-10T14:23:45Z",
      "title": "速度与激情全集1-10",
      "content": "速度与激情系列全集，1080P高清...",
      "links": [
        {
          "type": "baidu",
          "url": "https://pan.baidu.com/s/1abcdef",
          "password": "1234"
        }
      ],
      "tags": ["电影", "合集"]
    },
    // 更多结果...
  ],
  "merged_by_type": {
    "baidu": [
      {
        "url": "https://pan.baidu.com/s/1abcdef",
        "password": "1234",
        "note": "速度与激情全集1-10",
        "datetime": "2023-06-10T14:23:45Z"
      },
      // 更多百度网盘链接...
    ],
    "aliyun": [
      // 阿里云盘链接...
    ]
    // 更多网盘类型...
  }
}
```

**错误响应**：

```json
{
  "code": 400,
  "message": "关键词不能为空"
}
```

### 健康检查

检查API服务是否正常运行。

**接口地址**：`/api/health`  
**请求方法**：`GET`

**成功响应**：

```json
{
  "channels": [
    "tgsearchers2"
  ],
  "plugin_count": 6,
  "plugins": [
    "pansearch",
    "panta",
    "qupansou",
    "hunhepan",
    "jikepan",
    "pan666"
  ],
  "plugins_enabled": true,
  "status": "ok"
}
```

## 快速开始

### 环境要求

- Go 1.18+
- 可选：SOCKS5代理（用于访问受限地区的Telegram站点）

### 使用Docker部署

#### 方法1：使用Docker Compose（推荐）

1. 下载docker-compose.yml文件

```bash
wget https://raw.githubusercontent.com/fish2018/pansou/main/docker-compose.yml
```

2. 启动服务

```bash
docker-compose up -d
```

3. 访问服务

```
http://localhost:8888
```

#### 方法2：直接使用Docker命令

```bash
docker run -d --name pansou \
  -p 8888:8888 \
  -v pansou-cache:/app/cache \
  -e CHANNELS="tgsearchers2,SharePanBaidu,yunpanxunlei" \
  -e CACHE_ENABLED=true \
  -e ASYNC_PLUGIN_ENABLED=true \
  ghcr.io/fish2018/pansou:latest
```

### 从源码安装

1. 克隆仓库

```bash
git clone https://github.com/fish2018/pansou.git
cd pansou
```

2. 配置环境变量（可选）

| 环境变量 | 描述 | 默认值 |
|----------|------|--------|
| CHANNELS | 默认搜索频道列表（逗号分隔） | tgsearchers2 |
| CONCURRENCY | 默认并发数 | 频道数+插件数+10 |
| PORT | 服务端口 | 8888 |
| PROXY | SOCKS5代理 | - |
| CACHE_ENABLED | 是否启用缓存 | true |
| CACHE_PATH | 缓存文件路径 | ./cache |
| CACHE_MAX_SIZE | 最大缓存大小(MB) | 100 |
| CACHE_TTL | 缓存生存时间(分钟) | 60 |
| SHARD_COUNT | 缓存分片数量 | 8 |
| SERIALIZER_TYPE | 序列化器类型(gob/json) | gob |
| ENABLE_COMPRESSION | 是否启用压缩 | false |
| MIN_SIZE_TO_COMPRESS | 最小压缩阈值(字节) | 1024 |
| GC_PERCENT | GC触发百分比 | 100 |
| OPTIMIZE_MEMORY | 是否优化内存 | true |
| PLUGIN_TIMEOUT | 插件执行超时时间(秒) | 30 |
| ASYNC_PLUGIN_ENABLED | 是否启用异步插件 | true |
| ASYNC_RESPONSE_TIMEOUT | 异步响应超时时间(秒) | 4 |
| ASYNC_MAX_BACKGROUND_WORKERS | 最大后台工作者数量 | CPU核心数×5，最小20 |
| ASYNC_MAX_BACKGROUND_TASKS | 最大后台任务数量 | 工作者数量×5，最小100 |
| ASYNC_CACHE_TTL_HOURS | 异步缓存有效期(小时) | 1 |
| HTTP_READ_TIMEOUT | HTTP读取超时时间(秒) | 自动计算，最小30 |
| HTTP_WRITE_TIMEOUT | HTTP写入超时时间(秒) | 自动计算，最小60 |
| HTTP_IDLE_TIMEOUT | HTTP空闲连接超时时间(秒) | 120 |
| HTTP_MAX_CONNS | HTTP最大并发连接数 | CPU核心数×200，最小1000 |

```bash
# 默认频道
export CHANNELS="tgsearchers2,xxx"

# 缓存配置
export CACHE_ENABLED=true
export CACHE_PATH="./cache"
export CACHE_MAX_SIZE=100  # MB
export CACHE_TTL=60        # 分钟
export SHARD_COUNT=8       # 分片数量

# 异步插件配置
export ASYNC_PLUGIN_ENABLED=true
export ASYNC_RESPONSE_TIMEOUT=4            # 响应超时时间（秒）
export ASYNC_MAX_BACKGROUND_WORKERS=40     # 最大后台工作者数量
export ASYNC_MAX_BACKGROUND_TASKS=200      # 最大后台任务数量
export ASYNC_CACHE_TTL_HOURS=1             # 异步缓存有效期（小时）

# HTTP服务器配置
export HTTP_READ_TIMEOUT=30                # 读取超时时间（秒）
export HTTP_WRITE_TIMEOUT=60               # 写入超时时间（秒）
export HTTP_IDLE_TIMEOUT=120               # 空闲连接超时时间（秒）
export HTTP_MAX_CONNS=1600                 # 最大并发连接数（8核CPU示例）

# 代理配置（如需）
export PROXY="socks5://127.0.0.1:7890"
```

3. 构建

```linux
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -extldflags '-static'" -o pansou .
```

4. 运行

```bash
./pansou
```

### 其他配置参考

`supervisor`配置参考

```
[program:pansou]
environment=PORT=8888,CHANNELS="SharePanBaidu,yunpanxunlei,tianyifc,BaiduCloudDisk,txtyzy,peccxinpd,gotopan,xingqiump4,yunpanqk,PanjClub,kkxlzy,baicaoZY,MCPH01,share_aliyun,pan115_share,bdwpzhpd,ysxb48,pankuake_share,jdjdn1111,yggpan,yunpanall,MCPH086,zaihuayun,Q66Share,NewAliPan,Oscar_4Kmovies,ucwpzy,alyp_TV,alyp_4K_Movies,shareAliyun,alyp_1,yunpanpan,hao115,yunpanshare,dianyingshare,Quark_Movies,XiangxiuNB,NewQuark,ydypzyfx,kuakeyun,ucquark,xx123pan,yingshifenxiang123,zyfb123,pan123pan,tyypzhpd,tianyirigeng,cloudtianyi,hdhhd21,Lsp115,oneonefivewpfx,Maidanglaocom,qixingzhenren,taoxgzy,tgsearchers115,Channel_Shares_115,tyysypzypd,vip115hot,wp123zy,yunpan139,yunpan189,yunpanuc,yydf_hzl,alyp_Animation,alyp_JLP,tgsearchers2,leoziyuan"
command=/home/work/pansou/pansou
directory=/home/work/pansou
autostart=true
autorestart=true
startsecs=5
startretries=3
exitcodes=0
stopwaitsecs=10
stopasgroup=true
killasgroup=true
```

`nginx`配置参考

```
server {
    listen 80;
    server_name pansou.252035.xyz;

    # 将 HTTP 重定向到 HTTPS
    return 301 https://$host$request_uri;
}

limit_req_zone $binary_remote_addr zone=api_limit:10m rate=60r/m;

server {
    listen 443 ssl http2;
    server_name pansou.252035.xyz;

    access_log /home/work/logs/pansou.log;

    # 证书和密钥路径
    ssl_certificate /etc/letsencrypt/live/252035.xyz/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/252035.xyz/privkey.pem;

    # 增强 SSL 安全性
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers EECDH+AESGCM:EDH+AESGCM:AES256+EECDH:AES256+EDH;
    ssl_prefer_server_ciphers on;

    # 后端代理，应用限流
    location / {
        # 应用限流规则
        limit_req zone=api_limit burst=10 nodelay;
        # 当超过限制时返回 429 状态码
        limit_req_status 429;

        proxy_pass http://127.0.0.1:8888;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

## ⭐ Star 历史

[![Star History Chart](https://api.star-history.com/svg?repos=fish2018/pansou&type=Date)](https://star-history.com/#fish2018/pansou&Date)