package utils

import (
	"encoding/json"
	"strings"
	"testing"
)

const singboxScriptFixture = `
async function main(env) {
	const config = JSON.parse(env.files[0]);
	const proxies = await produceArtifact();
	config.outbounds = config.outbounds.concat(proxies);
	$content = JSON.stringify(config, null, 2);
}

await main({ arguments: $arguments, files: $files, producer: produceArtifact });
`

func TestRunSingboxScriptMatchesSubStoreSemantics(t *testing.T) {
	template := `{"outbounds":[{"type":"selector","tag":"pick","outbounds":[]}],"route":{"final":"pick"}}`
	proxies := `[{"tag":"HK 01","type":"shadowsocks","server":"1.2.3.4","server_port":8388,"method":"aes-128-gcm","password":"p"}]`

	out, err := RunSingboxScript(singboxScriptFixture, template, `{"profile":"company"}`, proxies)
	if err != nil {
		t.Fatalf("RunSingboxScript 失败: %v", err)
	}

	var config struct {
		Outbounds []map[string]any `json:"outbounds"`
		Route     map[string]any   `json:"route"`
	}
	if err := json.Unmarshal([]byte(out), &config); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if len(config.Outbounds) != 2 {
		t.Fatalf("出站数量 = %d, 期望 2", len(config.Outbounds))
	}
	if config.Outbounds[1]["tag"] != "HK 01" {
		t.Errorf("节点出站 tag = %v, 期望 HK 01", config.Outbounds[1]["tag"])
	}
	if config.Route["final"] != "pick" {
		t.Errorf("route.final = %v, 期望 pick", config.Route["final"])
	}
	// 缩进必须与 V8 的 JSON.stringify(x, null, 2) 一致。
	if !strings.Contains(out, "\n  \"outbounds\": [") {
		t.Errorf("缩进与 V8 不一致:\n%s", out)
	}
}

func TestRunSingboxScriptRejectsInvalidInput(t *testing.T) {
	if _, err := RunSingboxScript(singboxScriptFixture, "{}", "not-json", "[]"); err == nil {
		t.Error("参数不是合法 JSON 时应返回错误")
	}
	if _, err := RunSingboxScript(singboxScriptFixture, "{}", "{}", "not-json"); err == nil {
		t.Error("节点负载不是合法 JSON 时应返回错误")
	}
}

func TestIsSingboxTemplateScript(t *testing.T) {
	if !IsSingboxTemplateScript(singboxScriptFixture) {
		t.Error("应识别为 Sub-Store 风格脚本")
	}
	if IsSingboxTemplateScript("function subMod(input, clientType) { return input; }") {
		t.Error("普通 subMod 脚本不应识别为渲染脚本")
	}
}
