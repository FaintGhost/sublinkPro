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
		if outbound, ok := item.(map[string]any); ok {
			if tag, _ := outbound["tag"].(string); tag != "" {
				tags[tag] = true
			}
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
func ProxyToSingbox(p Proxy) (map[string]any, error) {
	if strings.TrimSpace(p.Name) == "" {
		return nil, fmt.Errorf("节点名称为空")
	}
	port := int(p.Port)
	outbound := map[string]any{
		"type":   p.Type,
		"tag":    p.Name,
		"server": p.Server,
	}
	if port > 0 {
		outbound["server_port"] = port
	}

	switch strings.ToLower(p.Type) {
	case "ss":
		outbound["type"] = "shadowsocks"
		outbound["method"] = p.Cipher
		outbound["password"] = p.Password
		if plugin, pluginOpts, ok := singboxShadowSocksPlugin(p); ok {
			outbound["plugin"] = plugin
			outbound["plugin_opts"] = pluginOpts
		} else if p.Plugin != "" {
			return nil, fmt.Errorf("sing-box 不支持 Shadowsocks 插件 %s", p.Plugin)
		}
	case "vmess":
		outbound["uuid"] = p.Uuid
		security := p.Cipher
		if security == "" {
			security = "auto"
		}
		outbound["security"] = security
		if aid, err := strconv.Atoi(p.AlterId); err == nil && aid != 0 {
			outbound["alter_id"] = aid
		}
		outbound["packet_encoding"] = "xudp"
		if tls := singboxTLS(p, p.Tls); tls != nil {
			outbound["tls"] = tls
		}
		if transport := singboxTransport(p); transport != nil {
			outbound["transport"] = transport
		}
	case "vless":
		outbound["uuid"] = p.Uuid
		if p.Flow != "" {
			outbound["flow"] = p.Flow
		}
		encoding := p.Packet_encoding
		if encoding == "" {
			encoding = "xudp"
		}
		outbound["packet_encoding"] = encoding
		if tls := singboxTLS(p, p.Tls); tls != nil {
			outbound["tls"] = tls
		}
		if transport := singboxTransport(p); transport != nil {
			outbound["transport"] = transport
		}
	case "trojan":
		outbound["password"] = p.Password
		outbound["tls"] = singboxTLS(p, true)
		if transport := singboxTransport(p); transport != nil {
			outbound["transport"] = transport
		}
	case "hysteria2", "hy2":
		outbound["type"] = "hysteria2"
		password := p.Password
		if password == "" {
			password = p.Auth
		}
		outbound["password"] = password
		if p.Up > 0 {
			outbound["up_mbps"] = int(p.Up)
		}
		if p.Down > 0 {
			outbound["down_mbps"] = int(p.Down)
		}
		if p.Obfs_password != "" {
			outbound["obfs"] = map[string]any{"type": "salamander", "password": p.Obfs_password}
		}
		if ports := singboxPorts(p); ports != nil {
			outbound["server_ports"] = ports
			delete(outbound, "server_port")
		}
		outbound["tls"] = singboxTLS(p, true)
	case "tuic":
		outbound["uuid"] = p.Uuid
		outbound["password"] = p.Password
		if p.Congestion_controller != "" {
			outbound["congestion_control"] = p.Congestion_controller
		}
		if p.Udp_relay_mode != "" {
			outbound["udp_relay_mode"] = p.Udp_relay_mode
		}
		outbound["tls"] = singboxTLS(p, true)
	case "anytls":
		outbound["password"] = p.Password
		if p.AnyTLSIdleCheck > 0 {
			outbound["idle_session_check_interval"] = fmt.Sprintf("%ds", p.AnyTLSIdleCheck)
		}
		if p.AnyTLSIdleTimeout > 0 {
			outbound["idle_session_timeout"] = fmt.Sprintf("%ds", p.AnyTLSIdleTimeout)
		}
		if p.AnyTLSMinIdle > 0 {
			outbound["min_idle_session"] = p.AnyTLSMinIdle
		}
		outbound["tls"] = singboxTLS(p, true)
	case "socks5", "socks":
		outbound["type"] = "socks"
		outbound["version"] = "5"
		if p.Username != "" {
			outbound["username"] = p.Username
		}
		if p.Password != "" {
			outbound["password"] = p.Password
		}
	case "http", "https":
		outbound["type"] = "http"
		if p.Username != "" {
			outbound["username"] = p.Username
		}
		if p.Password != "" {
			outbound["password"] = p.Password
		}
		if p.Type == "https" || p.Tls {
			outbound["tls"] = singboxTLS(p, true)
		}
	default:
		return nil, fmt.Errorf("sing-box 不支持协议 %s", p.Type)
	}

	if port <= 0 && outbound["server_ports"] == nil {
		return nil, fmt.Errorf("端口无效")
	}
	return outbound, nil
}

// singboxTLS 构造 sing-box TLS 出站字段；enabled 为 false 时返回 nil。
func singboxTLS(p Proxy, enabled bool) map[string]any {
	if !enabled {
		return nil
	}
	tls := map[string]any{"enabled": true}

	serverName := p.Servername
	if serverName == "" {
		serverName = p.Sni
	}
	if serverName != "" {
		tls["server_name"] = serverName
	}
	if p.Skip_cert_verify {
		tls["insecure"] = true
	}
	if len(p.Alpn) > 0 {
		tls["alpn"] = p.Alpn
	}

	fingerprint := p.Client_fingerprint
	if len(p.Reality_opts) > 0 {
		if fingerprint == "" {
			fingerprint = "chrome"
		}
		reality := map[string]any{"enabled": true}
		if pbk, ok := p.Reality_opts["public-key"].(string); ok && pbk != "" {
			reality["public_key"] = pbk
		}
		if sid, ok := p.Reality_opts["short-id"].(string); ok && sid != "" {
			reality["short_id"] = sid
		}
		tls["reality"] = reality
	}
	if fingerprint != "" {
		tls["utls"] = map[string]any{"enabled": true, "fingerprint": fingerprint}
	}
	if len(p.ECH_opts) > 0 {
		ech := map[string]any{"enabled": true}
		if value, ok := p.ECH_opts["config"].(string); ok && value != "" {
			ech["config"] = value
		}
		tls["ech"] = ech
	}
	return tls
}

// singboxTransport 构造 sing-box 传输层字段。
func singboxTransport(p Proxy) map[string]any {
	network := strings.ToLower(strings.TrimSpace(p.Network))
	switch network {
	case "ws", "websocket":
		transport := map[string]any{"type": "ws"}
		path := ""
		var headers map[string]any
		if p.Ws_opts != nil {
			path, _ = p.Ws_opts["path"].(string)
			headers, _ = p.Ws_opts["headers"].(map[string]any)
			if upgrade, ok := p.Ws_opts["v2ray-http-upgrade"].(bool); ok && upgrade {
				transport["type"] = "httpupgrade"
			}
			if early, ok := p.Ws_opts["max-early-data"]; ok {
				transport["max_early_data"] = early
			}
			if header, ok := p.Ws_opts["early-data-header-name"].(string); ok && header != "" {
				transport["early_data_header_name"] = header
			}
		}
		if path != "" {
			transport["path"] = path
		}
		if host := headerHost(headers); host != "" {
			transport["headers"] = map[string]any{"Host": host}
		}
		return transport
	case "grpc":
		transport := map[string]any{"type": "grpc"}
		if p.Grpc_opts != nil {
			if service, ok := p.Grpc_opts["grpc-service-name"].(string); ok && service != "" {
				transport["service_name"] = service
			}
		}
		return transport
	case "h2", "http":
		transport := map[string]any{"type": "http"}
		opts := p.H2_opts
		if network == "http" {
			opts = p.Http_opts
			if method, ok := opts["method"].(string); ok && method != "" {
				transport["method"] = method
			}
			if host := headerHost(opts["headers"].(map[string]any), true); host != "" {
				transport["host"] = []string{host}
			}
		} else if host := stringList(opts["host"]); len(host) > 0 {
			transport["host"] = host
		}
		if path := stringList(opts["path"]); len(path) > 0 {
			transport["path"] = path[0]
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
	raw = strings.ReplaceAll(raw, ",", "/")
	parts := strings.Split(raw, "/")
	ports := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(strings.ReplaceAll(part, ":", "-"))
		if part == "" {
			continue
		}
		ports = append(ports, part)
	}
	if len(ports) == 0 {
		return nil
	}
	return ports
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
