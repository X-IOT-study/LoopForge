package httpapi

import (
	"fmt"
	"net/http"
	"strings"

	"agentflow/internal/config"
	"agentflow/internal/domain"
	"agentflow/internal/repo"

	"github.com/gin-gonic/gin"
)

// localModeExecutors is the built-in executor catalog with the CLIs the
// current mode cannot run removed. Platform mode returns the catalog
// untouched, so the executors endpoint and bootstrap always hand the page the
// same set it can actually start.
func localModeExecutors() []domain.ExecutorDefinition {
	all := repo.BuiltInExecutors()
	if !config.LocalMode() {
		return all
	}
	kept := make([]domain.ExecutorDefinition, 0, len(all))
	for _, executor := range all {
		// The catalog carries both the executor id and its CLI kind; the
		// switch is defined on the kind (kimi/zcode ids equal their kinds).
		if config.LocalModeSupportsExecutor(executor.Kind) {
			kept = append(kept, executor)
		}
	}
	return kept
}

// respondLocalModeExecutorUnsupported rejects a role or a generic template
// that pins a CLI local mode withdrew. It returns true when it wrote the
// rejection, so callers can stop; names the CLI because the caller chose it.
func respondLocalModeExecutorUnsupported(c *gin.Context, kind string) bool {
	if config.LocalModeSupportsExecutor(kind) {
		return false
	}
	kind = strings.TrimSpace(kind)
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
		"error": fmt.Sprintf("本地模式不支持执行器 %s（本地模式不提供 %s）", kind, strings.Join(config.LocalModeUnsupportedExecutors(), "、")),
		"code":  config.LocalModeErrorCode,
	})
	return true
}
