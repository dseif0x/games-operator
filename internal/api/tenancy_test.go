package api

import (
	"net/http"
	"net/http/cookiejar"
	"testing"
)

// login returns a client logged in as the user.
func login(t *testing.T, base string, s *Server, username, password string) *client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: base, http: &http.Client{Jar: jar}, server: s}
	code, out := c.do("POST", "/api/v1/auth/login", map[string]string{"username": username, "password": password})
	if code != 200 {
		t.Fatalf("login %s: %d %v", username, code, out)
	}
	c.csrf, _ = out["csrf"].(string)
	return c
}

func TestAdminAndUsers(t *testing.T) {
	srv, _ := newServerAt(t, "", nil)
	admin := login(t, srv.URL, nil, "admin", "secret")
	code, me := admin.do("GET", "/api/v1/auth/me", nil)
	if code != 200 || me["user"].(map[string]any)["role"] != "admin" {
		t.Fatalf("me: %d %v", code, me)
	}

	// Users are created by admins, with a role and a quota.
	code, out := admin.do("POST", "/api/v1/users", map[string]any{"username": "Bob", "password": "short"})
	if code != 400 {
		t.Fatalf("short password: %d %v", code, out)
	}
	code, out = admin.do("POST", "/api/v1/users", map[string]any{"username": "Bob", "password": "bobsecret1", "quota": map[string]any{"max_apps": 1}})
	if code != 201 || out["username"] != "bob" || out["role"] != "user" {
		t.Fatalf("create user: %d %v", code, out)
	}
	bobID := out["id"].(string)
	if code, _ := admin.do("POST", "/api/v1/users", map[string]any{"username": "bob", "password": "bobsecret1"}); code != 409 {
		t.Fatalf("duplicate user: %d", code)
	}
	code, out = admin.do("GET", "/api/v1/users", nil)
	if code != 200 || len(out["users"].([]any)) != 2 {
		t.Fatalf("list users: %d %v", code, out)
	}

	bob := login(t, srv.URL, nil, "bob", "bobsecret1")
	// Everything admin is closed to bob.
	for _, r := range []struct{ method, path string }{
		{"GET", "/api/v1/users"}, {"POST", "/api/v1/users"}, {"PATCH", "/api/v1/users/" + bobID}, {"DELETE", "/api/v1/users/" + bobID},
		{"POST", "/api/v1/catalog"}, {"PATCH", "/api/v1/catalog/x"}, {"DELETE", "/api/v1/catalog/x"},
		{"GET", "/api/v1/admin/apps"}, {"POST", "/api/v1/admin/apps/x/stop"}, {"DELETE", "/api/v1/admin/apps/x"},
	} {
		if code, out := bob.do(r.method, r.path, map[string]any{}); code != 403 {
			t.Fatalf("%s %s as bob: %d %v", r.method, r.path, code, out)
		}
	}
	// A custom app is an admin's thing; the catalog is for everyone.
	if code, out := bob.do("POST", "/api/v1/apps", map[string]any{"name": "Mine", "preset": "steam"}); code != 403 {
		t.Fatalf("custom app as bob: %d %v", code, out)
	}
	code, tpl := admin.do("POST", "/api/v1/catalog", map[string]any{"name": "Steam", "preset": "steam", "description": "Big Picture"})
	if code != 201 || tpl["enabled"] != true {
		t.Fatalf("create template: %d %v", code, tpl)
	}
	code, out = bob.do("GET", "/api/v1/catalog", nil)
	if code != 200 || len(out["templates"].([]any)) != 1 {
		t.Fatalf("catalog as bob: %d %v", code, out)
	}
	code, app := bob.do("POST", "/api/v1/apps", map[string]any{"template_id": tpl["id"], "name": "Bob's Steam"})
	if code != 201 || app["template_id"] != tpl["id"] || app["name"] != "Bob's Steam" {
		t.Fatalf("instance as bob: %d %v", code, app)
	}
	if code, out := bob.do("GET", "/api/v1/apps", nil); code != 200 || out["apps"].([]any)[0].(map[string]any)["template_name"] != "Steam" {
		t.Fatalf("bob's list: %d %v", code, out)
	}
	if code, out := bob.do("POST", "/api/v1/apps", map[string]any{"template_id": tpl["id"], "name": "Another"}); code != 409 {
		t.Fatalf("quota: %d %v", code, out)
	}
	if code, out := bob.do("PATCH", "/api/v1/apps/"+app["id"].(string), map[string]any{"name": "x", "preset": "firefox"}); code != 403 {
		t.Fatalf("edit as bob: %d %v", code, out)
	}
	// The admin sees it in the overview, with the owner.
	code, out = admin.do("GET", "/api/v1/admin/apps", nil)
	if code != 200 || out["apps"].([]any)[0].(map[string]any)["owner"] != "bob" {
		t.Fatalf("overview: %d %v", code, out)
	}
	if code, _ := admin.do("GET", "/api/v1/apps/"+app["id"].(string), nil); code != 404 {
		t.Fatalf("the admin's own list is their own: %d", code)
	}

	// Passwords: bob changes his own, the admin resets it.
	if code, _ := bob.do("POST", "/api/v1/me/password", map[string]any{"current_password": "wrong", "new_password": "bobsecret2"}); code != 403 {
		t.Fatalf("wrong current password: %d", code)
	}
	if code, _ := bob.do("POST", "/api/v1/me/password", map[string]any{"current_password": "bobsecret1", "new_password": "bobsecret2"}); code != 200 {
		t.Fatalf("change password: %d", code)
	}
	login(t, srv.URL, nil, "bob", "bobsecret2")
	if code, out := admin.do("PATCH", "/api/v1/users/"+bobID, map[string]any{"password": "bobsecret3", "disabled": true}); code != 200 || out["disabled"] != true {
		t.Fatalf("admin update: %d %v", code, out)
	}
	if code, _ := bob.do("GET", "/api/v1/auth/me", nil); code != 401 {
		t.Fatalf("a disabled user is logged out: %d", code)
	}
	// The admin cannot lock themselves out.
	adminID := me["user"].(map[string]any)["id"].(string)
	if code, _ := admin.do("PATCH", "/api/v1/users/"+adminID, map[string]any{"role": "user"}); code != 400 {
		t.Fatalf("self demotion: %d", code)
	}
	if code, _ := admin.do("DELETE", "/api/v1/users/"+adminID, nil); code != 400 {
		t.Fatalf("self delete: %d", code)
	}
	if code, _ := admin.do("DELETE", "/api/v1/users/"+bobID, nil); code != 200 {
		t.Fatalf("delete bob: %d", code)
	}
	if code, out := admin.do("GET", "/api/v1/admin/apps", nil); code != 200 || len(out["apps"].([]any)) != 0 {
		t.Fatalf("bob's apps go with bob: %v", out)
	}
}
