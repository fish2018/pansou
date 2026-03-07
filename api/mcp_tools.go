package api

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	McpMethodNameSearch = "search"
	McpMethodNameHealth = "health"
)

// SetupMcpTool 设置MCP工具
func SetupMcpTool() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "pansou", Version: "v2.0"}, nil)
	server.AddReceivingMiddleware(McpAuthMiddleware)
	mcp.AddTool(server, &mcp.Tool{Name: McpMethodNameHealth, Description: "获取服务器状态"}, func(_ context.Context, request *mcp.CallToolRequest, input map[string]any) (toolCallResult *mcp.CallToolResult, result HealthResponse, _ error) {
		result = Health()
		return
	})
	mcp.AddTool(server, &mcp.Tool{Name: McpMethodNameSearch, Description: "搜索网盘资源"}, SearchMcpHandler)

	return server
}
