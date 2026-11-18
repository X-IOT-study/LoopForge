package httpapi

import (
	"net/http"
	"strings"

	"agentflow/internal/config"

	"github.com/gin-gonic/gin"
)

// localModeBlockedPrefixes are the platform-only capabilities a local-mode
// instance cannot serve: notification bots, platform accounts and admins, the
// SSO exchange, API tokens, the MCP/skill marketplaces, knowledge submissions
// and upstream sync, and the runner knowledge queue. The list mirrors
// tech-design §5 one to one; paths that merely share a module root (runs,
// agents, issues, projects, schedule jobs) stay reachable.
var localModeBlockedPrefixes = []string{
	"/api/notification-bots",
	"/api/notification-preview",
	"/api/notification-deliveries",
	"/api/bot-bindings",
	"/api/platform/",
	"/api/auth/gongfeng",
	"/api/auth/exchange",
	"/api/auth/tokens",
	"/api/mcp-market",
	"/api/skillhub/",
	"/api/knowledge/submissions",
	"/api/sync/",
	"/api/shared-schedule-tasks",
	"/runners/knowledge/",
}

// localModeBlockedPath reports whether a request targets one of them. Three
// entries are method- or shape-specific because their siblings are local
// features: listing accounts reaches the platform while a single account's
// credentials stay usable, publishing a schedule job hands it to the platform
// while editing it does not, and a runner bind code enrolls a remote machine.
func localModeBlockedPath(method, path string) bool {
	for _, prefix := range localModeBlockedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	// /api/users GET is deliberately absent: the assistant, the quick-access
	// wizard and every member picker load it on first paint, and rejecting it
	// kills their whole bootstrap. Local mode serves the single local account
	// instead (usersHandlers.users), so the list stays truthful without
	// exposing account administration.
	switch path {
	case "/api/runners/bind-codes":
		return method == http.MethodPost
	}
	return strings.HasPrefix(path, "/api/schedule-jobs/") && strings.HasSuffix(path, "/publish")
}

// respondLocalModeUnsupported is the one rejection shape for local mode: the page
// switches a feature off on "code", and the text says why.
func respondLocalModeUnsupported(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "该能力在本地模式下不可用", "code": config.LocalModeErrorCode})
}

// localModeGuard rejects platform-only routes before anything else looks at
// the caller: the answer does not depend on who asks, so an anonymous request
// must get 403 rather than the 401 that userAccessAuth would produce.
func localModeGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !config.LocalMode() {
			c.Next()
			return
		}
		if localModeBlockedPath(c.Request.Method, c.Request.URL.Path) {
			respondLocalModeUnsupported(c)
			return
		}
		c.Next()
	}
}
