package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"sublink/cache"
	"sublink/utils"

	"gopkg.in/yaml.v3"
)

// AllProxiesPlaceholder 与 Clash 模板保持一致：代理组内出现该占位符时展开为全部节点。
const AllProxiesPlaceholder = "__ALL_PROXIES__"

// clashGroupTypeToSingbox 将 Clash 代理组类型映射为 sing-box 出站类型。
// sing-box 没有 fallback / load-balance，统一降级为 urltest（自动选择可用节点）。
func clashGroupTypeToSingbox(clashType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(clashType)) {
	case "select":
		return "selector", true
	case "url-test", "fallback", "load-balance":
		return "urltest", true
	default:
		return "", false
	}
}

// EncodeSingbox 将 Clash 配置（模板 + 节点 + 代理组 + 已执行脚本）转换为 sing-box 配置。
//
// clashConfig: DecodeClash 产出的 Clash YAML（脚本已执行）
// templatePath: sing-box 模板（JSON 文件路径或 URL），其中 outbounds 为模板自带的固定出站
// （direct / block / dns-out 等），可用 __ALL_PROXIES__ 引用全部节点。
//
// 合并规则：模板出站（含占位符展开）→ Clash 代理组转换出的 selector/urltest → 节点出站。
func EncodeSingbox(clashConfig []byte, templatePath string) ([]byte, error) {
	var clash struct {
		Proxies     []Proxy          `yaml:"proxies"`
		ProxyGroups []map[string]any `yaml:"proxy-groups"`
	}
	if err := yaml.Unmarshal(clashConfig, &clash); err != nil {
		return nil, fmt.Errorf("解析 Clash 配置失败: %w", err)
	}

	templateBody, err := loadSingboxTemplate(templatePath)
	if err != nil {
		return nil, err
	}
	var config map[string]any
	if err := json.Unmarshal(templateBody, &config); err != nil {
		return nil, fmt.Errorf("解析 sing-box 模板失败: %w", err)
	}

	// 节点出站
	nodeOutbounds := make([]any, 0, len(clash.Proxies))
	nodeTags := make([]string, 0, len(clash.Proxies))
	for _, p := range clash.Proxies {
		outbound, err := ProxyToSingbox(p)
		if err != nil {
			utils.Warn("sing-box 跳过节点 %s: %v", p.Name, err)
			continue
		}
		nodeOutbounds = append(nodeOutbounds, outbound)
		nodeTags = append(nodeTags, p.Name)
	}

	usedTags := map[string]bool{}
	var merged []any

	// 模板自带出站
	for _, item := range anySlice(config["outbounds"]) {
		outbound, ok := item.(map[string]any)
		if !ok {
			continue
		}
		tag, _ := outbound["tag"].(string)
		expandPlaceholderInOutbound(outbound, nodeTags)
		applyTemplateGroupFilter(outbound, nodeTags)
		if tag != "" && usedTags[tag] {
			continue
		}
		if tag != "" {
			usedTags[tag] = true
		}
		merged = append(merged, outbound)
	}

	directTag, blockTag := findSystemTags(config["outbounds"])

	// Clash 代理组 → sing-box selector/urltest
	for _, group := range clash.ProxyGroups {
		groupOutbound, tag, ok := clashGroupToSingbox(group, clash.Proxies, directTag, blockTag)
		if !ok {
			continue
		}
		if usedTags[tag] {
			continue
		}
		usedTags[tag] = true
		merged = append(merged, groupOutbound)
	}

	merged = append(merged, nodeOutbounds...)
	merged = dropDanglingMembers(merged)
	config["outbounds"] = merged

	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化 sing-box 配置失败: %w", err)
	}
	return encoded, nil
}

// dropDanglingMembers 移除指向不存在出站的成员，避免 sing-box 启动时报错。
func dropDanglingMembers(outbounds []any) []any {
	tags := make(map[string]bool, len(outbounds))
	for _, item := range outbounds {
		if tag := outboundTag(item); tag != "" {
			tags[tag] = true
		}
	}
	for _, item := range outbounds {
		outbound, ok := item.(map[string]any)
		if !ok {
			continue
		}
		items := anySlice(outbound["outbounds"])
		if len(items) == 0 {
			continue
		}
		kept := make([]string, 0, len(items))
		for _, member := range items {
			name := fmt.Sprint(member)
			if tags[name] {
				kept = append(kept, name)
				continue
			}
			utils.Warn("sing-box 代理组 %v 的成员 %s 不存在，已移除", outbound["tag"], name)
		}
		outbound["outbounds"] = targetStringSlice(kept)
	}
	return outbounds
}

// outboundTag 读取出站的 tag，兼容有序对象与普通 map。
func outboundTag(item any) string {
	switch typed := item.(type) {
	case *jsonObject:
		if value, ok := typed.Get("tag"); ok {
			tag, _ := value.(string)
			return tag
		}
	case map[string]any:
		tag, _ := typed["tag"].(string)
		return tag
	}
	return ""
}


// loadSingboxTemplate 读取模板内容，本地文件走模板缓存，与 DecodeClash 行为一致。
func loadSingboxTemplate(templatePath string) ([]byte, error) {
	if templatePath == "" {
		return nil, fmt.Errorf("sing-box 模板路径为空")
	}
	if strings.Contains(templatePath, "://") {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, templatePath, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		return io.ReadAll(resp.Body)
	}
	filename := filepath.Base(templatePath)
	if cached, ok := cache.GetTemplateContent(filename); ok {
		return []byte(cached), nil
	}
	data, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("读取 sing-box 模板失败: %w", err)
	}
	cache.SetTemplateContent(filename, string(data))
	return data, nil
}

// applyTemplateGroupFilter 按模板里声明的 filter 规则给代理组补齐节点成员。
//
// 语义与 Sub-Store 模板脚本一致：保留原有成员，把匹配到的节点追加到末尾。
//
//	"filter": {"include": "🇭🇰|香港|HK", "exclude": ["landing", "game"]}
//
// include 为不区分大小写的正则；exclude 为不区分大小写的关键字列表；两者同时为空则不处理。
func applyTemplateGroupFilter(outbound map[string]any, nodeTags []string) {
	raw, ok := outbound["filter"]
	if !ok {
		return
	}
	delete(outbound, "filter")
	spec, ok := raw.(map[string]any)
	if !ok {
		return
	}
	include, _ := spec["include"].(string)
	excludes := stringList(spec["exclude"])
	if strings.TrimSpace(include) == "" && len(excludes) == 0 {
		return
	}

	var includeRe *regexp.Regexp
	if strings.TrimSpace(include) != "" {
		compiled, err := regexp.Compile("(?i)" + include)
		if err != nil {
			utils.Warn("sing-box 模板 filter 正则无效 %q: %v", include, err)
			return
		}
		includeRe = compiled
	}

	members := make([]string, 0, len(nodeTags)+4)
	for _, member := range anySlice(outbound["outbounds"]) {
		members = append(members, fmt.Sprint(member))
	}
	for _, tag := range nodeTags {
		if includeRe != nil && !includeRe.MatchString(tag) {
			continue
		}
		if excludedByKeywords(tag, excludes) {
			continue
		}
		members = append(members, tag)
	}
	outbound["outbounds"] = targetStringSlice(dedupeStrings(members))
}

// excludedByKeywords 判断节点名是否命中排除关键字。
func excludedByKeywords(tag string, keywords []string) bool {
	lowered := strings.ToLower(tag)
	for _, keyword := range keywords {
		keyword = strings.ToLower(strings.TrimSpace(keyword))
		if keyword == "" {
			continue
		}
		if strings.Contains(lowered, keyword) {
			return true
		}
	}
	return false
}

// ReadTemplateFile 读取模板内容，供渲染流程使用。
func ReadTemplateFile(templatePath string) ([]byte, error) {
	return loadSingboxTemplate(templatePath)
}

// EncodeSingboxProxies 把 Clash 配置里的节点转换成 sing-box 出站数组（JSON 文本）。
// 字段顺序与 Sub-Store 的转换结果一致，便于逐字节比对。
func EncodeSingboxProxies(clashConfig []byte) ([]byte, error) {
	var clash struct {
		Proxies []Proxy `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(clashConfig, &clash); err != nil {
		return nil, fmt.Errorf("解析 Clash 配置失败: %w", err)
	}
	outbounds := make([]any, 0, len(clash.Proxies))
	for _, p := range clash.Proxies {
		outbound, err := ProxyToSingbox(p)
		if err != nil {
			utils.Warn("sing-box 跳过节点 %s: %v", p.Name, err)
			continue
		}
		outbounds = append(outbounds, outbound)
	}
	return json.Marshal(outbounds)
}

// findSystemTags 找出模板里的 direct / block 出站 tag，供代理组成员名重写使用。
func findSystemTags(outbounds any) (directTag, blockTag string) {
	directTag = "direct"
	for _, item := range anySlice(outbounds) {
		outbound, ok := item.(map[string]any)
		if !ok {
			continue
		}
		tag, _ := outbound["tag"].(string)
		switch outbound["type"] {
		case "direct":
			if tag != "" {
				directTag = tag
			}
		case "block", "reject":
			if tag != "" {
				blockTag = tag
			}
		}
	}
	return directTag, blockTag
}

// clashGroupToSingbox 将单个 Clash 代理组转换为 sing-box 出站。
func clashGroupToSingbox(group map[string]any, nodes []Proxy, directTag, blockTag string) (map[string]any, string, bool) {
	name, _ := group["name"].(string)
	if name == "" {
		return nil, "", false
	}
	outType, ok := clashGroupTypeToSingbox(fmt.Sprint(group["type"]))
	if !ok {
		utils.Warn("sing-box 跳过代理组 %s: 不支持的类型 %v", name, group["type"])
		return nil, "", false
	}

	targets := make([]string, 0, len(nodes)+4)
	for _, member := range anySlice(group["proxies"]) {
		memberName := fmt.Sprint(member)
		switch memberName {
		case AllProxiesPlaceholder:
			targets = append(targets, nodeTagsOf(nodes)...)
		case "DIRECT", "direct":
			targets = append(targets, directTag)
		case "REJECT", "REJECT-DROP", "block", "blocked":
			// sing-box 1.12 起没有 block 出站，模板未声明时直接丢弃这些成员。
			if blockTag != "" {
				targets = append(targets, blockTag)
			}
		default:
			targets = append(targets, memberName)
		}
	}
	// include-all / filter / exclude-* 组：mihomo 在客户端侧过滤，sing-box 没有等价能力，这里提前算好成员。
	targets = append(targets, filterNodeTags(group, nodes)...)
	targets = dedupeStrings(targets)
	if len(targets) == 0 {
		utils.Warn("sing-box 跳过代理组 %s: 成员为空", name)
		return nil, "", false
	}

	outbound := map[string]any{
		"type":      outType,
		"tag":       name,
		"outbounds": targetStringSlice(targets),
	}
	if outType == "urltest" {
		url, _ := group["url"].(string)
		if url == "" {
			url = "http://www.gstatic.com/generate_204"
		}
		outbound["url"] = url
		outbound["interval"] = durationString(group["interval"], 300)
		if group["tolerance"] != nil {
			// sing-box 的 tolerance 是毫秒整数，不是时长字符串。
			outbound["tolerance"] = intValue(group["tolerance"], 50)
		}
	}
	return outbound, name, true
}

// dedupeStrings 按出现顺序去重。
func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

// nodeTagsOf 返回节点名称列表。
func nodeTagsOf(nodes []Proxy) []string {
	tags := make([]string, 0, len(nodes))
	for _, n := range nodes {
		tags = append(tags, n.Name)
	}
	return tags
}

// filterNodeTags 按 mihomo 的 include-all / filter / exclude-filter / exclude-type 规则筛选节点。
func filterNodeTags(group map[string]any, nodes []Proxy) []string {
	includeAll, _ := group["include-all"].(bool)
	filter, _ := group["filter"].(string)
	excludeFilter, _ := group["exclude-filter"].(string)
	excludeType, _ := group["exclude-type"].(string)
	if !includeAll && strings.TrimSpace(filter) == "" {
		return nil
	}

	var include, exclude *regexp.Regexp
	if strings.TrimSpace(filter) != "" {
		compiled, err := regexp.Compile(filter)
		if err != nil {
			utils.Warn("sing-box 代理组 %v 的 filter 无效: %v", group["name"], err)
			return nil
		}
		include = compiled
	}
	if strings.TrimSpace(excludeFilter) != "" {
		compiled, err := regexp.Compile(excludeFilter)
		if err != nil {
			utils.Warn("sing-box 代理组 %v 的 exclude-filter 无效: %v", group["name"], err)
			return nil
		}
		exclude = compiled
	}

	excludedTypes := map[string]bool{}
	for _, t := range strings.Split(excludeType, "|") {
		if t = strings.TrimSpace(t); t != "" {
			excludedTypes[strings.ToLower(t)] = true
		}
	}

	matched := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if excludedTypes[strings.ToLower(n.Type)] {
			continue
		}
		if include != nil && !include.MatchString(n.Name) {
			continue
		}
		if exclude != nil && exclude.MatchString(n.Name) {
			continue
		}
		matched = append(matched, n.Name)
	}
	return matched
}

// expandPlaceholderInOutbound 展开模板出站里的 __ALL_PROXIES__ 占位符。
func expandPlaceholderInOutbound(outbound map[string]any, nodeTags []string) {
	items := anySlice(outbound["outbounds"])
	if len(items) == 0 {
		return
	}
	replaced := make([]string, 0, len(items))
	for _, item := range items {
		name := fmt.Sprint(item)
		if name == AllProxiesPlaceholder {
			replaced = append(replaced, nodeTags...)
			continue
		}
		replaced = append(replaced, name)
	}
	outbound["outbounds"] = targetStringSlice(replaced)
}

// ProxyToSingbox 将通用 Proxy 转换为 sing-box 出站结构。
//
// 字段顺序与 Sub-Store 的转换结果保持一致，便于逐字节比对。
func ProxyToSingbox(p Proxy) (*jsonObject, error) {
	if strings.TrimSpace(p.Name) == "" {
		return nil, fmt.Errorf("节点名称为空")
	}
	port := int(p.Port)
	out := newJSONObject()
	out.Set("tag", p.Name)

	switch strings.ToLower(p.Type) {
	case "ss":
		out.Set("type", "shadowsocks")
		out.Set("server", p.Server)
		out.Set("server_port", port)
		out.Set("method", p.Cipher)
		out.Set("password", p.Password)
		if plugin, pluginOpts, ok := singboxShadowSocksPlugin(p); ok {
			out.Set("plugin", plugin)
			out.Set("plugin_opts", pluginOpts)
		}
	case "vmess":
		out.Set("type", "vmess")
		out.Set("server", p.Server)
		out.Set("server_port", port)
		out.Set("uuid", p.Uuid)
		security := p.Cipher
		if security == "" {
			security = "auto"
		}
		out.Set("security", security)
		if aid, err := strconv.Atoi(p.AlterId); err == nil && aid != 0 {
			out.Set("alter_id", aid)
		}
		if tls := singboxTLS(p, p.Tls); tls != nil {
			out.Set("tls", tls)
		}
		if transport := singboxTransport(p); transport != nil {
			out.Set("transport", transport)
		}
	case "vless":
		out.Set("type", "vless")
		out.Set("server", p.Server)
		out.Set("server_port", port)
		out.Set("uuid", p.Uuid)
		if tls := singboxTLS(p, p.Tls); tls != nil {
			out.Set("tls", tls)
		}
		packetEncoding := p.Packet_encoding
		if packetEncoding == "" {
			packetEncoding = "xudp"
		}
		out.Set("packet_encoding", packetEncoding)
		if p.Flow != "" {
			out.Set("flow", p.Flow)
		}
		if transport := singboxTransport(p); transport != nil {
			out.Set("transport", transport)
		}
	case "trojan":
		out.Set("type", "trojan")
		out.Set("server", p.Server)
		out.Set("server_port", port)
		out.Set("password", p.Password)
		out.Set("tls", singboxTLS(p, true))
		if transport := singboxTransport(p); transport != nil {
			out.Set("transport", transport)
		}
	case "hysteria2", "hy2":
		out.Set("type", "hysteria2")
		out.Set("server", p.Server)
		out.Set("server_port", port)
		password := p.Password
		if password == "" {
			password = p.Auth
		}
		out.Set("password", password)
		out.Set("tls", singboxTLS(p, true))
		if ports := singboxPorts(p); ports != nil {
			out.Set("server_ports", ports)
			// 端口跳跃的链接没有独立端口时，沿用区间起始端口，保持与 Sub-Store 一致。
			if port == 0 {
				if base := basePortOf(ports); base > 0 {
					out.Set("server_port", base)
				}
			}
		}
		if p.Up > 0 {
			out.Set("up_mbps", int(p.Up))
		}
		if p.Down > 0 {
			out.Set("down_mbps", int(p.Down))
		}
	case "tuic":
		out.Set("type", "tuic")
		out.Set("server", p.Server)
		out.Set("server_port", port)
		out.Set("uuid", p.Uuid)
		out.Set("password", p.Password)
		if p.Congestion_controller != "" {
			out.Set("congestion_control", p.Congestion_controller)
		}
		if p.Udp_relay_mode != "" {
			out.Set("udp_relay_mode", p.Udp_relay_mode)
		}
		out.Set("tls", singboxTLS(p, true))
	case "anytls":
		out.Set("type", "anytls")
		out.Set("server", p.Server)
		out.Set("server_port", port)
		out.Set("password", p.Password)
		out.Set("tls", singboxTLS(p, true))
		if p.AnyTLSMinIdle > 0 {
			out.Set("min_idle_session", p.AnyTLSMinIdle)
		}
	case "socks5", "socks":
		out.Set("type", "socks")
		out.Set("server", p.Server)
		out.Set("server_port", port)
		out.Set("version", "5")
		if p.Username != "" {
			out.Set("username", p.Username)
		}
		if p.Password != "" {
			out.Set("password", p.Password)
		}
	case "http", "https":
		out.Set("type", "http")
		out.Set("server", p.Server)
		out.Set("server_port", port)
		if p.Username != "" {
			out.Set("username", p.Username)
		}
		if p.Password != "" {
			out.Set("password", p.Password)
		}
		if p.Type == "https" || p.Tls {
			out.Set("tls", singboxTLS(p, true))
		}
	default:
		return nil, fmt.Errorf("sing-box 不支持协议 %s", p.Type)
	}

	if port <= 0 {
		if _, ok := out.Get("server_ports"); !ok {
			return nil, fmt.Errorf("端口无效")
		}
	}
	return out, nil
}

// singboxTLS 构造 sing-box TLS 出站字段；enabled 为 false 时返回 nil。
func singboxTLS(p Proxy, enabled bool) *jsonObject {
	if !enabled {
		return nil
	}
	tls := newJSONObject()
	tls.Set("enabled", true)

	serverName := p.Servername
	if serverName == "" {
		serverName = p.Sni
	}
	if serverName != "" {
		tls.Set("server_name", serverName)
	}
	tls.Set("insecure", p.Skip_cert_verify)
	if len(p.Alpn) > 0 {
		tls.Set("alpn", p.Alpn)
	}

	fingerprint := p.Client_fingerprint
	if len(p.Reality_opts) > 0 {
		if fingerprint == "" {
			fingerprint = "chrome"
		}
		reality := newJSONObject()
		reality.Set("enabled", true)
		if pbk, ok := p.Reality_opts["public-key"].(string); ok && pbk != "" {
			reality.Set("public_key", pbk)
		}
		if sid, ok := p.Reality_opts["short-id"].(string); ok && sid != "" {
			reality.Set("short_id", sid)
		}
		tls.Set("reality", reality)
	}
	if fingerprint != "" {
		utls := newJSONObject()
		utls.Set("enabled", true)
		utls.Set("fingerprint", fingerprint)
		tls.Set("utls", utls)
	}
	if len(p.ECH_opts) > 0 {
		ech := newJSONObject()
		ech.Set("enabled", true)
		if value, ok := p.ECH_opts["config"].(string); ok && value != "" {
			ech.Set("config", value)
		}
		tls.Set("ech", ech)
	}
	return tls
}

// singboxTransport 构造 sing-box 传输层字段。
func singboxTransport(p Proxy) *jsonObject {
	network := strings.ToLower(strings.TrimSpace(p.Network))
	switch network {
	case "ws", "websocket":
		transport := newJSONObject()
		path := ""
		var headers map[string]any
		if p.Ws_opts != nil {
			path, _ = p.Ws_opts["path"].(string)
			headers, _ = p.Ws_opts["headers"].(map[string]any)
			if upgrade, ok := p.Ws_opts["v2ray-http-upgrade"].(bool); ok && upgrade {
				transport.Set("type", "httpupgrade")
			} else {
				transport.Set("type", "ws")
			}
			if early, ok := p.Ws_opts["max-early-data"]; ok {
				transport.Set("max_early_data", early)
			}
			if header, ok := p.Ws_opts["early-data-header-name"].(string); ok && header != "" {
				transport.Set("early_data_header_name", header)
			}
		}
		if host := headerHost(headers); host != "" {
			transport.Set("headers", map[string]any{"Host": host})
		}
		if path != "" {
			transport.Set("path", path)
		}
		return transport
	case "grpc":
		transport := newJSONObject()
		transport.Set("type", "grpc")
		if p.Grpc_opts != nil {
			if service, ok := p.Grpc_opts["grpc-service-name"].(string); ok && service != "" {
				transport.Set("service_name", service)
			}
		}
		return transport
	case "h2", "http":
		transport := newJSONObject()
		transport.Set("type", "http")
		opts := p.H2_opts
		if network == "http" {
			opts = p.Http_opts
			if method, ok := opts["method"].(string); ok && method != "" {
				transport.Set("method", method)
			}
			if raw, ok := opts["headers"].(map[string]any); ok {
				if host := headerHost(raw, true); host != "" {
					transport.Set("host", []string{host})
				}
			}
		} else if host := stringList(opts["host"]); len(host) > 0 {
			transport.Set("host", host)
		}
		if path := stringList(opts["path"]); len(path) > 0 {
			transport.Set("path", path[0])
		}
		return transport
	default:
		return nil
	}
}

// singboxPorts 解析 hysteria2 端口跳跃串为 sing-box server_ports 格式。
func singboxPorts(p Proxy) []string {
	raw := strings.TrimSpace(p.Ports)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	ports := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// sing-box 的端口区间用冒号表示。
		ports = append(ports, strings.ReplaceAll(part, "-", ":"))
	}
	if len(ports) == 0 {
		return nil
	}
	return ports
}

// basePortOf 取端口跳跃区间里的起始端口。
func basePortOf(ports []string) int {
	if len(ports) == 0 {
		return 0
	}
	first := strings.SplitN(ports[0], ":", 2)[0]
	value, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		return 0
	}
	return value
}

// singboxShadowSocksPlugin 将 mihomo 的 SS 插件配置转换为 sing-box plugin/plugin_opts。
func singboxShadowSocksPlugin(p Proxy) (string, string, bool) {
	switch strings.ToLower(strings.TrimSpace(p.Plugin)) {
	case "":
		return "", "", false
	case "obfs", "obfs-local", "simple-obfs":
		mode, _ := p.Plugin_opts["mode"].(string)
		host, _ := p.Plugin_opts["host"].(string)
		if mode == "" {
			mode = "http"
		}
		opts := "obfs=" + mode
		if host != "" {
			opts += ";obfs-host=" + host
		}
		return "obfs-local", opts, true
	case "v2ray-plugin":
		mode, _ := p.Plugin_opts["mode"].(string)
		host, _ := p.Plugin_opts["host"].(string)
		path, _ := p.Plugin_opts["path"].(string)
		opts := []string{"mode=websocket"}
		if tlsEnabled, ok := p.Plugin_opts["tls"].(bool); ok && tlsEnabled {
			opts = append(opts, "tls")
		}
		if host != "" {
			opts = append(opts, "host="+host)
		}
		if path != "" {
			opts = append(opts, "path="+path)
		}
		if mode == "" {
			opts = []string{"mode=websocket"}
		}
		return "v2ray-plugin", strings.Join(opts, ";"), true
	default:
		return "", "", false
	}
}

func anySlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []string:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item)
		}
		return items
	default:
		return nil
	}
}

func targetStringSlice(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func stringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			result = append(result, fmt.Sprint(item))
		}
		return result
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	default:
		return nil
	}
}

func headerHost(headers map[string]any, valuesAreList ...bool) string {
	if headers == nil {
		return ""
	}
	raw, ok := headers["Host"]
	if !ok {
		raw, ok = headers["host"]
	}
	if !ok {
		return ""
	}
	if len(valuesAreList) > 0 && valuesAreList[0] {
		if list := stringList(raw); len(list) > 0 {
			return list[0]
		}
	}
	return fmt.Sprint(raw)
}

// intValue 将模板里的数字/数字字符串解析为 int。
func intValue(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			return parsed
		}
	}
	return fallback
}

// durationString 将 Clash 的秒数间隔转换为 sing-box 需要的时长字符串。
func durationString(value any, fallback int) string {
	seconds := fallback
	switch typed := value.(type) {
	case int:
		seconds = typed
	case int64:
		seconds = int(typed)
	case float64:
		seconds = int(typed)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(typed)); err == nil {
			seconds = parsed
		}
	}
	if seconds <= 0 {
		seconds = fallback
	}
	return fmt.Sprintf("%ds", seconds)
}
