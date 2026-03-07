# PanSou MCP 服务文档

## 功能介绍

PanSou MCP 服务是一个基于 [Model Context Protocol (MCP)](https://modelcontextprotocol.io) 的工具服务，它将 PanSou 网盘搜索 API 的功能封装为可在支持 MCP 的客户端（如 Cherry Studio）中直接调用的工具。

通过 PanSou MCP 服务，可以直接在 Claude 等 AI 助手中搜索网盘资源，极大地提升了获取网盘资源的便捷性。

### 核心功能

1. **搜索网盘资源 (`search_netdisk`)**:
   - 支持通过 `keyword` 参数搜索网盘资源。
   - 可通过 `source_type` 参数指定搜索来源：Telegram 频道、插件或两者结合。
   - 可通过 `cloud_types` 参数过滤结果，显示特定类型的网盘链接。
   - 支持通过 `force_refresh` 参数请求后端刷新缓存。
   - 支持通过 `ext_params` 参数向后端插件传递扩展参数。
   - 支持通过 `result_type` 参数控制后端返回的结果格式。
   - 支持通过 `concurrency` 参数指定并发搜索数量。

2. **检查服务健康状态 (`check_service_health`)**:
   - 检查所连接的 PanSou 后端服务是否正常运行。
   - 获取后端服务的配置信息，如可用的 Telegram 频道列表和插件列表。

3. **启动后端服务 (`start_backend`)**:
   - 自动启动本地的 PanSou Go 后端服务（如果尚未运行）。
   - 等待服务完全启动并可用后才开始处理其他请求。
   - 支持参数：`force_restart`（可选，布尔值，是否强制重启后端服务，默认为false）。

4. **获取静态资源信息 (`pansou://` URI scheme)**:
   - 提供可用插件列表、可用频道列表和支持的网盘类型列表等静态信息资源。
   - 支持资源URI：`pansou://plugins`（插件列表）、`pansou://channels`（频道列表）、`pansou://cloud-types`（网盘类型列表）。

### 架构与部署方式

PanSou MCP 服务设计为与 PanSou Go 后端服务分离，通过 HTTP API 进行通信。支持以下部署方式：

- **Node.js 部署 (TypeScript)**: MCP 服务基于 TypeScript 开发，编译后通过 `node` 命令运行编译后的 JavaScript 文件。它会自动连接到指定的 PanSou 后端服务。
- **Docker 部署**: 使用 Docker 容器运行 PanSou 后端服务，MCP 服务通过 HTTP API 连接到容器化的后端。

---

## 安装与部署

### 前提条件

1. **Node.js**: 确保您的系统已安装 Node.js (版本 >= 18.0.0)。您可以通过在终端运行 `node -v` 来检查版本。
2. **Go**: 确保您的系统已安装 Go (版本 >= 1.18)。您可以通过在终端运行 `go version` 来检查版本。

### 部署步骤

PanSou 后端服务通常运行在 `http://localhost:8888` (默认地址)。支持以下两种后端部署方式：

## 后端服务部署

### 方式一：源码部署后端服务

- 确保系统已安装 Go 1.23.0 或更高版本。
- 克隆或确保已有 PanSou Go 项目源码。
- 在项目根目录下，打开终端并执行以下命令进行构建：

```bash
# Windows (PowerShell/CMD)
go build -o pansou.exe .
```

- 构建完成后，运行生成的可执行文件以启动后端服务：

```bash
# Windows
.\pansou.exe
```

服务默认将在 `http://localhost:8888` 启动。

### 方式二：Docker 部署后端服务

Docker 部署方式更加简单，无需手动构建 Go 后端服务，直接使用预构建的 Docker 镜像。

**前提条件**：确保您的系统已安装 Docker 和 Docker Compose。

在 PanSou 项目根目录下，使用 Docker Compose 启动后端服务：

```bash
# 启动 Docker 容器
docker-compose up -d

# 检查容器状态
docker ps

# 验证服务是否正常运行
curl http://localhost:8888/api/health
```

### 验证后端服务

无论使用哪种方式启动后端服务，您都可以通过访问 `http://localhost:8888/api/health` 来检查服务状态，应该能看到类似以下的 JSON 响应：

```json
{
  "status": "ok",
  "plugins_enabled": true,
  "channels_count": 1,
  "channels": ["tgsearchers3"],
  "plugin_count": 38,
  "plugins": ["ddys", "erxiao", "..."]
}
```

---

## MCP 服务配置与使用

### 1. 构建 MCP 服务

- 确保系统已安装 Node.js (版本 >= 18.0.0)。
- 在 `typescript` 目录下，打开终端并执行以下命令来安装依赖并构建项目：

```bash
cd typescript
npm install
npm run build
```

构建完成后，编译后的 JavaScript 文件将位于 `typescript/dist` 目录下。

### 2. MCP 服务运行方式

构建完成后，可以通过以下方式之一运行 MCP 服务：

- **在MCP调用时自动启动** (推荐):
  直接配置MCP客户端，调用时会自动启动后端服务器。

- **使用 `node` 直接运行** (手动启动):
  在 PanSou 项目根目录下（包含 `typescript` 文件夹），运行：

  ```bash
  # Windows (CMD/PowerShell)
  node .\typescript\dist\index.js
  ```

服务启动后，将默认尝试连接到 `http://localhost:8888` 的 PanSou 后端服务。

如果想要后端服务运行在不同的地址或端口上，需要通过环境变量指定：

```bash
# Windows (CMD)
set PANSOU_SERVER_URL=http://your-backend-address:port
node .\typescript\dist\index.js

# Windows (PowerShell)
$env:PANSOU_SERVER_URL='http://your-backend-address:port'
node .\typescript\dist\index.js
```

### 3. MCP 客户端配置

#### 示例配置 Cherry Studio(版本1.5.7)

要在 Cherry Studio 中使用 PanSou MCP 服务，需要将其添加到 Cherry Studio MCP 的配置文件中。

- 找到 设置中的MCP。
- 选择 `添加服务器` 、 `从JSON导入` 。
- 加入服务配置(可以直接复制项目根目录下的 `mcp-config.json` 内容)：

```json
{
  "mcpServers": {
    "pansou": {
      "command": "node",
      "args": [
        "C:\\full\\path\\to\\your\\project\\typescript\\dist\\index.js"
      ],
      "env": {
        "PANSOU_SERVER_URL": "http://localhost:8888",
        "REQUEST_TIMEOUT": "30",
        "MAX_RESULTS": "50",
        "DEFAULT_CLOUD_TYPES": "baidu,aliyun,quark,tianyi,uc,mobile,115,pikpak,xunlei,123,magnet,ed2k,others",
        "AUTO_START_BACKEND": "true",
        "DOCKER_MODE": "false",
        "BACKEND_SHUTDOWN_DELAY": "5000",
        "BACKEND_STARTUP_TIMEOUT": "30000",
        "IDLE_TIMEOUT": "300000",
        "ENABLE_IDLE_SHUTDOWN": "true",
        "PROJECT_ROOT_PATH": "C:\\full\\path\\to\\your\\project",
        "ENABLED_PLUGINS": "labi,zhizhen,shandian,duoduo,muou,wanou"
      }
    }
  }
}
```

- 添加streamable方式的MCP
```json
{
  "mcpServers": {
    "pansou": {
      "type": "streamableHttp",
      "description": "",
      "isActive": true,
      "baseUrl": "http://localhost:8888/mcp",
      "headers": {
        "Authorization": "your-mcp-token"
      }
    }
  }
}
```

**注意**：
- 请将 `C:\\full\\path\\to\\your\\project` 替换为您项目实际的完整路径
- 如需强制指定部署模式，可修改 `DOCKER_MODE` 和 `AUTO_START_BACKEND` 参数
- **重要**：从当前版本开始，必须通过 `ENABLED_PLUGINS` 显式指定要启用的插件，否则不会启用任何插件
- **注意**：如果启动的时候指定了AUTH_MCP_TOKEN环境变量，则streamable的方式连接的时候才需要验证Authorization

### 4. 启动 MCP 服务并开始使用

配置完成后，在对话界面启用 PanSou MCP 服务，即可开始尝试搜索。

<img width="495" height="649" alt="image" src="https://github.com/user-attachments/assets/b8c72649-03e8-4f52-86ba-aa16c4cc3b7e" />

---

## 配置说明与高级选项

### 智能检测机制

当 `DOCKER_MODE` 设置为 `"false"` 或未设置时，MCP 服务将自动检测部署模式：

1. **Docker 容器检测**：检查是否有运行中的 Docker 容器（名称包含 "pansou"）
2. **源码部署检测**：检查是否存在 Go 可执行文件（pansou.exe/main.exe）
3. **服务运行检测**：检查后端服务是否已在运行

### 配置模式

- **自动模式**（推荐）：使用默认配置，让服务自动检测部署方式
- **强制 Docker 模式**：设置 `"DOCKER_MODE": "true"`
- **强制源码模式**：设置 `"DOCKER_MODE": "false"` 且 `"AUTO_START_BACKEND": "true"`
- **仅连接模式**：设置 `"AUTO_START_BACKEND": "false"`（适用于手动启动的后端）

### 统一配置文件

无论使用哪种后端部署方式，都可以使用统一的 `mcp-config.json` 配置文件。MCP 服务会根据配置自动检测和适配不同的部署模式。

### 常见问题排查

#### 后端服务连接问题

1. **检查服务状态**：
   ```bash
   # 检查健康状态
   curl http://localhost:8888/api/health
   
   # 或使用 PowerShell
   Invoke-WebRequest -Uri "http://localhost:8888/api/health"
   ```

2. **Docker 部署问题**：
   ```bash
   # 检查容器状态
   docker ps
   
   # 查看容器日志
   docker-compose logs
   
   # 重启容器
   docker-compose restart
   ```

3. **源码部署问题**：
   - 确认 Go 版本 >= 1.25.0
   - 检查端口 8888 是否被占用
   - 确认防火墙设置

4. **MCP 服务问题**：
   - 确认 Node.js 版本 >= 18.0.0
   - 检查 `typescript/dist` 目录是否存在
   - 验证配置文件中的路径是否正确

---

## 支持的参数

MCP 服务通过工具调用接收参数。以下是主要工具及其支持的参数：

### `search_netdisk` 工具

用于搜索网盘资源。

| 参数名          | 类型            | 必填 | 默认值               | 描述                                                         |
| :-------------- | :-------------- | :--- | :------------------- | :----------------------------------------------------------- |
| `keyword`       | string          | 是   | -                    | 搜索关键词，例如 "速度与激情"、"Python教程"。                |
| `channels`      | array of string | 否   | 配置默认值           | 要搜索的 Telegram 频道列表，例如 `["tgsearchers3", "another_channel"]`。 |
| `plugins`       | array of string | 否   | 配置默认值或所有插件 | 要使用的搜索插件列表，例如 `["pansearch", "panta"]`。        |
| `cloud_types`   | array of string | 否   | 无过滤               | 过滤结果，仅返回指定类型的网盘链接。支持的类型有：`baidu`, `aliyun`, `quark`, `tianyi`, `uc`, `mobile`, `115`, `pikpak`, `xunlei`, `123`, `magnet`, `ed2k`, `others`。 |
| `source_type`   | string          | 否   | `"all"`              | 数据来源类型。可选值：`"all"` (全部来源), `"tg"` (仅 Telegram), `"plugin"` (仅插件)。 |
| `force_refresh` | boolean         | 否   | `false`              | 是否强制刷新缓存，以获取最新数据。                           |
| `result_type`   | string          | 否   | `"merge"`            | 返回结果的类型。可选值：`"all"` (返回所有结果), `"results"` (仅返回详细结果), `"merge"` (仅返回按网盘类型分组的结果)。 |
| `concurrency`   | number          | 否   | 自动计算             | 并发搜索的数量，0或不指定则自动计算。                         |
| `ext_params`    | object          | 否   | `{}`                 | 传递给后端插件的自定义扩展参数，例如 `{"title_en": "Fast and Furious", "is_all": true}`。 |

---

### `check_service_health` 工具

用于检查后端服务健康状态。

- **参数**: 无

---

### `start_backend` 工具

用于启动本地 PanSou 后端服务。

| 参数名          | 类型    | 必填 | 默认值  | 描述                                       |
| :-------------- | :------ | :--- | :------ | :----------------------------------------- |
| `force_restart` | boolean | 否   | `false` | 是否强制重启后端服务（即使它已经在运行）。 |

---

### 环境变量配置

您可以通过设置环境变量来配置 MCP 服务的行为：

| 环境变量               | 描述                                                       | 默认值                    |
| :--------------------- | :--------------------------------------------------------- | :------------------------ |
| `PANSOU_SERVER_URL`    | PanSou 后端服务的 URL 地址。                               | `http://localhost:8888`   |
| `REQUEST_TIMEOUT`      | HTTP 请求超时时间（秒）。                                  | `30`                      |
| `MAX_RESULTS`          | （内部使用，限制处理结果数量）                             | `100`                     |
| `DEFAULT_CHANNELS`     | 默认搜索的 Telegram 频道列表（逗号分隔）。                 | `""` (使用后端默认)       |
| `DEFAULT_PLUGINS`      | 默认使用的搜索插件列表（逗号分隔）。                       | `""` (使用后端默认或所有) |
| `ENABLED_PLUGINS`      | 指定后端启用的插件列表（逗号分隔），必须显式指定。         | `""` (需要显式设置)       |
| `DEFAULT_CLOUD_TYPES`  | 默认的网盘类型过滤器（逗号分隔）。                         | `""` (无过滤)             |
| `AUTO_START_BACKEND`   | 是否在 MCP 服务启动时自动尝试启动后端服务。                | `true`                    |
| `DOCKER_MODE`          | 部署模式控制。设置为 `true` 强制使用 Docker 模式；设置为 `false` 或未设置时启用智能检测。智能检测将自动识别 Docker 容器、源码部署或运行中的服务。 | `false` (智能检测)        |
| `PROJECT_ROOT_PATH`    | PanSou 后端可执行文件所在的目录路径（用于自动启动）。      | 无                        |
| `IDLE_TIMEOUT`         | 空闲超时时间（毫秒），超过此时间无活动则可能关闭后端服务。 | `300000` (5分钟)          |
| `ENABLE_IDLE_SHUTDOWN` | 是否启用空闲超时自动关闭后端服务。                         | `true`                    |
