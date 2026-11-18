package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"agentflow/internal/domain"
)

// TestLocalModeUserListReturnsOnlyTheSignedInAccount pins the local-mode
// contract of GET /api/users: one account, the caller's own, and an empty list
// when the request carries no identity (the endpoint must never widen to the
// full roster).
func TestLocalModeUserListReturnsOnlyTheSignedInAccount(t *testing.T) {
	d := deps{}
	items := []domain.User{
		{ID: 1, LoginName: "local"},
		{ID: 2, LoginName: "someone-else"},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	req = req.WithContext(context.WithValue(req.Context(), requestUserContextKey{}, domainUserToAuth(items[1])))

	got := d.localModeUserList(req, items)
	if len(got) != 1 || got[0].ID != 2 || got[0].LoginName != "someone-else" {
		t.Fatalf("localModeUserList() = %+v, want only the signed-in account", got)
	}
}

// TestLocalModeUserListWithoutIdentityIsEmpty proves an anonymous request gets
// no roster at all rather than the whole store.
func TestLocalModeUserListWithoutIdentityIsEmpty(t *testing.T) {
	d := deps{}
	items := []domain.User{{ID: 1, LoginName: "local"}}
	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)

	if got := d.localModeUserList(req, items); len(got) != 0 {
		t.Fatalf("localModeUserList() without identity = %+v, want empty", got)
	}
}

// TestLocalModeUserListIgnoresUnknownAccount covers a stale session: the
// cookie parses but the account is gone, so nothing is returned.
func TestLocalModeUserListIgnoresUnknownAccount(t *testing.T) {
	d := deps{}
	req := httptest.NewRequest(http.MethodGet, "/api/users", nil)
	req = req.WithContext(context.WithValue(req.Context(), requestUserContextKey{}, domainUserToAuth(domain.User{ID: 99, LoginName: "gone"})))

	if got := d.localModeUserList(req, []domain.User{{ID: 1, LoginName: "local"}}); len(got) != 0 {
		t.Fatalf("localModeUserList() for an unknown account = %+v, want empty", got)
	}
}
