package xray

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
)

func testConfig(t *testing.T, cfgPath string) config.Config {
	t.Helper()
	return config.Config{ServerHost: "vpn.example.com", Xray: config.XrayConfig{EnableDirectConfig: true, ConfigPaths: []string{cfgPath}, InboundTag: "inbound-dragoncore", Protocol: "vless", LinkNetwork: "xhttp", LinkSecurity: "tls", LinkPort: 443, DragonCoreXrayProtocol: "xhttp", RestartCommand: "", AccessLogPaths: []string{filepath.Join(t.TempDir(), "access.log")}}}
}

func writeSampleConfig(t *testing.T, path string) {
	t.Helper()
	data := map[string]any{"inbounds": []any{map[string]any{"tag": "other", "protocol": "vless", "port": float64(8443), "settings": map[string]any{"clients": []any{}}, "streamSettings": map[string]any{"network": "ws"}}, map[string]any{"tag": "inbound-dragoncore", "protocol": "vless", "port": float64(443), "settings": map[string]any{"clients": []any{}}, "streamSettings": map[string]any{"network": "xhttp", "security": "tls", "tlsSettings": map[string]any{"serverName": "sni.example.com"}, "xhttpSettings": map[string]any{"path": "/xhttp", "host": "host.example.com"}}}}}
	b, _ := json.MarshalIndent(data, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestApplyAccountUpsertsClientAndBuildsLink(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	writeSampleConfig(t, cfgPath)
	cfg := testConfig(t, cfgPath)
	cfg.Xray.EnableDragonCorePG = false
	m := NewManager(cfg)
	acc := model.Account{Username: "Cliente1", UUID: "A0B1C2D3-E4F5-4678-9ABC-DEF012345678", ExpiryDate: "2026-07-02", LimitConnections: 1}
	res, err := m.ApplyAccount(context.Background(), acc, ApplyOptions{NoRestart: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.UUID != "a0b1c2d3-e4f5-4678-9abc-def012345678" {
		t.Fatalf("uuid normalizado errado: %s", res.UUID)
	}
	if !strings.Contains(res.Link, "vless://a0b1c2d3-e4f5-4678-9abc-def012345678@vpn.example.com:443") {
		t.Fatalf("link inesperado: %s", res.Link)
	}
	if !strings.Contains(res.Link, "host=host.example.com") || !strings.Contains(res.Link, "path=%2Fxhttp") || !strings.Contains(res.Link, "sni=sni.example.com") {
		t.Fatalf("link sem params esperados: %s", res.Link)
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"email": "Cliente1"`) {
		t.Fatalf("client não gravado: %s", string(b))
	}
	if !strings.Contains(string(b), `"access":`) {
		t.Fatalf("access.log não garantido: %s", string(b))
	}
}

func TestUpsertCorrectsExistingEmailAndBlocksDuplicateUUID(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeSampleConfig(t, cfgPath)
	cfg := testConfig(t, cfgPath)
	m := NewManager(cfg)
	_, data, err := m.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	uuid := "a0b1c2d3-e4f5-4678-9abc-def012345678"
	changed, err := m.UpsertClient(data, "Cliente1", uuid)
	if err != nil || !changed {
		t.Fatalf("primeiro upsert: changed=%v err=%v", changed, err)
	}
	changed, err = m.UpsertClient(data, "Cliente1", "b0b1c2d3-e4f5-4678-9abc-def012345678")
	if err != nil || !changed {
		t.Fatalf("correção uuid: changed=%v err=%v", changed, err)
	}
	_, err = m.UpsertClient(data, "Cliente2", "b0b1c2d3-e4f5-4678-9abc-def012345678")
	if err == nil {
		t.Fatal("uuid duplicado em outro usuário foi aceito")
	}
}

func TestRemoveClient(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeSampleConfig(t, cfgPath)
	cfg := testConfig(t, cfgPath)
	m := NewManager(cfg)
	_, data, err := m.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	uuid := "a0b1c2d3-e4f5-4678-9abc-def012345678"
	_, _ = m.UpsertClient(data, "Cliente1", uuid)
	changed, err := m.RemoveClient(data, "Cliente1", "")
	if err != nil || !changed {
		t.Fatalf("remove changed=%v err=%v", changed, err)
	}
	in := m.SelectInbound(data)
	settingsObj := in["settings"].(map[string]any)
	clients := settingsObj["clients"].([]any)
	if len(clients) != 0 {
		t.Fatalf("client não removido: %#v", clients)
	}
}

func TestTestConfigSkipsWhenNoCLI(t *testing.T) {
	ok, info := TestConfig(context.Background(), filepath.Join(t.TempDir(), "config.json"))
	if !ok {
		t.Fatalf("sem CLI deveria ser skip ok: %s", info)
	}
}

var _ = time.Now
