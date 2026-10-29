package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const singboxTestClash = `proxies:
  - {name: "HK 01", type: ss, server: 1.2.3.4, port: 8388, cipher: aes-128-gcm, password: pass}
  - {name: "JP Reality", type: vless, server: 5.6.7.8, port: 443, uuid: 11111111-2222-3333-4444-555555555555, tls: true, servername: aws.amazon.com, flow: xtls-rprx-vision, client-fingerprint: chrome, reality-opts: {public-key: PUBKEY, short-id: 5b9e2c7a4d1f8e30}}
proxy-groups:
  - {name: "🚀 节点选择", type: select, proxies: ["♻️ 自动选择", "direct-test"]}
  - {name: "♻️ 自动选择", type: url-test, url: "http://www.gstatic.com/generate_204", interval: 300, tolerance: 50, proxies: ["__ALL_PROXIES__"]}
  - {name: "🎯 直连", type: select, proxies: [DIRECT, REJECT]}
`

const singboxTestTemplate = `{
  "outbounds": [
    {"type": "selector", "tag": "节点选择", "outbounds": ["__ALL_PROXIES__", "direct"]},
    {"type": "direct", "tag": "direct"},
    {"type": "block", "tag": "block"}
  ],
  "route": {"final": "节点选择"}
}`

func TestEncodeSingbox(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "singbox.json")
	if err := os.WriteFile(templatePath, []byte(singboxTestTemplate), 0o644); err != nil {
		t.Fatal(err)
	}

	body, err := EncodeSingbox([]byte(singboxTestClash), templatePath)
	if err != nil {
		t.Fatalf("EncodeSingbox 失败: %v", err)
	}

	var config struct {
		Outbounds []map[string]any `json:"outbounds"`
		Route     map[string]any   `json:"route"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if got := config.Route["final"]; got != "节点选择" {
		t.Errorf("route.final = %v, 期望 节点选择", got)
	}

	byTag := map[string]map[string]any{}
	for _, outbound := range config.Outbounds {
		tag, _ := outbound["tag"].(string)
		if _, exists := byTag[tag]; exists {
			t.Errorf("出站 tag 重复: %s", tag)
		}
		byTag[tag] = outbound
	}

	// 模板出站
	if byTag["direct"]["type"] != "direct" {
		t.Errorf("模板 direct 出站丢失")
	}
	// 模板 selector 的占位符展开为全部节点
	members := outboundTags(t, byTag["节点选择"])
	if len(members) != 3 || members[0] != "HK 01" || members[2] != "direct" {
		t.Errorf("节点选择 成员 = %v, 期望 [HK 01 JP Reality direct]", members)
	}
	// Clash 代理组转换
	urltest := byTag["♻️ 自动选择"]
	if urltest["type"] != "urltest" || urltest["url"] == nil || urltest["interval"] != "300s" || urltest["tolerance"] != float64(50) {
		t.Errorf("urltest 出站不符合预期: %v", urltest)
	}
	if got := outboundTags(t, urltest); len(got) != 2 {
		t.Errorf("urltest 成员数 = %d, 期望 2", len(got))
	}
	// DIRECT/REJECT 重写
	direct := byTag["🎯 直连"]
	if got := outboundTags(t, direct); len(got) != 2 || got[0] != "direct" || got[1] != "block" {
		t.Errorf("直连组成员 = %v, 期望 [direct block]", got)
	}
	// 节点出站
	if byTag["HK 01"]["type"] != "shadowsocks" || byTag["HK 01"]["method"] != "aes-128-gcm" {
		t.Errorf("SS 出站转换错误: %v", byTag["HK 01"])
	}
	reality := byTag["JP Reality"]
	tls, _ := reality["tls"].(map[string]any)
	if tls == nil || tls["server_name"] != "aws.amazon.com" {
		t.Fatalf("vless TLS 缺失: %v", reality)
	}
	realityOpts, _ := tls["reality"].(map[string]any)
	if realityOpts == nil || realityOpts["public_key"] != "PUBKEY" || realityOpts["short_id"] != "5b9e2c7a4d1f8e30" {
		t.Errorf("REALITY 字段错误: %v", realityOpts)
	}
	utls, _ := tls["utls"].(map[string]any)
	if utls == nil || utls["fingerprint"] != "chrome" {
		t.Errorf("utls 字段错误: %v", utls)
	}
	if reality["flow"] != "xtls-rprx-vision" {
		t.Errorf("flow 丢失: %v", reality["flow"])
	}
}

func TestEncodeSingboxSkipsUnsupportedAndBadTemplate(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "singbox-alt.json")
	if err := os.WriteFile(templatePath, []byte(`{"outbounds":[{"type":"direct","tag":"direct"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	clash := "proxies:\n  - {name: \"SSR\", type: ssr, server: 1.1.1.1, port: 80, cipher: aes-128-cfb, password: x}\n"
	body, err := EncodeSingbox([]byte(clash), templatePath)
	if err != nil {
		t.Fatalf("EncodeSingbox 失败: %v", err)
	}
	var config struct {
		Outbounds []map[string]any `json:"outbounds"`
	}
	if err := json.Unmarshal(body, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Outbounds) != 1 {
		t.Errorf("不支持的协议应被跳过，实际出站数 = %d", len(config.Outbounds))
	}

	if _, err := EncodeSingbox([]byte(singboxTestClash), filepath.Join(dir, "missing.json")); err == nil {
		t.Error("模板不存在时应返回错误")
	}
}

func outboundTags(t *testing.T, outbound map[string]any) []string {
	t.Helper()
	if outbound == nil {
		t.Fatal("出站不存在")
	}
	raw, _ := outbound["outbounds"].([]any)
	tags := make([]string, 0, len(raw))
	for _, item := range raw {
		tags = append(tags, item.(string))
	}
	return tags
}
