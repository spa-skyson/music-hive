package apitest

import (
	"encoding/json"
	"testing"
)

// TestAuthDisabledLoginAndMe — MUSIC_HIVE_AUTH_DISABLED=1: логин отвечает
// auth:false, me — ok:true/auth_enabled:false, logout работает.
func TestAuthDisabledLoginAndMe(t *testing.T) {
	server, _, _ := openPGContractsServer(t)

	rec := serve(server, jsonReq("POST", "/api/auth/login", `{"username":"owner","password":"x"}`))
	if rec.Code != 200 {
		t.Fatalf("login status=%d", rec.Code)
	}
	var login map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatal(err)
	}
	if login["auth"] != false {
		t.Fatalf("expected auth=false when disabled: %#v", login)
	}

	rec = serve(server, jsonReq("GET", "/api/auth/me", ""))
	var me struct {
		OK          bool `json:"ok"`
		AuthEnabled bool `json:"auth_enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if !me.OK || me.AuthEnabled {
		t.Fatalf("me=%+v", me)
	}

	rec = serve(server, jsonReq("POST", "/api/auth/logout", ""))
	if rec.Code != 200 {
		t.Fatalf("logout status=%d", rec.Code)
	}
}
