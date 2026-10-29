# 配置模板

模板是订阅输出的骨架文件，节点与代理组在生成订阅时注入模板。

## 模板类别

| 类别 | 典型文件 | 作用 |
| --- | --- | --- |
| `clash` | `clash.yaml` | Clash / mihomo / Stash 配置 |
| `surge` | `surge.conf` | Surge 配置 |
| `singbox` | `singbox.json` | sing-box 配置 |

类别按扩展名推断：`.conf` → `surge`，`.json` → `singbox`，其余 → `clash`。模板文件放在工作目录的 `template/` 下，启动时自动入库。

## sing-box 模板

订阅请求带 `?client=sing-box`（或 `singbox`）时，服务端用 sing-box 模板渲染**完整配置**，不再依赖 Sub-Store 转换。

模板是一个普通 sing-box JSON 配置文件，合并规则：

1. **模板 `outbounds`** 原样保留（`direct`、`block`、选择器等）。其中 `outbounds` 数组里的 `__ALL_PROXIES__` 占位符展开为全部节点。
2. **Clash 模板的 `proxy-groups`** 转换成 sing-box 出站：`select` → `selector`，`url-test` / `fallback` / `load-balance` → `urltest`（`url`、`interval`、`tolerance` 同步）。成员里的 `DIRECT` 重写为模板的 `direct` 出站 tag，`REJECT` 重写为 `block` 出站 tag（模板没有 `block` 出站时丢弃该成员）。
3. **节点出站**追加在最后，tag 即节点名。

tag 冲突时模板优先。`route`、`dns`、`inbounds`、`experimental` 完全由模板决定。

### 支持的节点协议

`ss`（含 obfs-local / v2ray-plugin）、`vmess`、`vless`（REALITY、ws / grpc / h2 / httpupgrade）、`trojan`、`hysteria2`（含端口跳跃）、`tuic`、`anytls`、`socks5`、`http`。其余协议（ssr、snell、mieru、wireguard、openvpn）会跳过并写日志。

### 订阅配置

订阅的 `config` JSON 增加 `singbox` 键指向模板路径，例如：

```json
{"clash":"./template/clash.yaml","surge":"./template/surge.conf","singbox":"./template/singbox.json","udp":false,"cert":false}
```

未配置时回退到工作目录的 `template/singbox.json`；该模板不存在时退回 Sub-Store sidecar，只输出 `outbounds` 节点列表。

### 内置模板

`template/singbox.json` 提供可直接使用的起点：mixed 入站 7890、`节点选择` 选择器（默认选中全部节点）、直连/代理分流 DNS、`hijack-dns` 与私有地址直连规则。常用改动是换成 `tun` 入站、替换 DNS 上游、加 `rule_set` 分流。

## sing-box 渲染脚本

除了「模板 + 节点」的直接合并，订阅还可以挂一个 **Sub-Store Script Operator 风格** 的渲染脚本，由脚本产出最终配置。
这类脚本自带 `Runtime` / `ProxySource` / `Pipeline`，与宿主只通过四个约定变量交互：

| 变量 | 含义 |
| --- | --- |
| `$arguments` | 脚本参数（JSON 对象），由脚本的 `args` 字段提供 |
| `$files` | 模板文件数组，`$files[0]` 是订阅 `config.singbox` 指向的模板内容 |
| `produceArtifact` | 宿主提供的函数，返回当前订阅的节点出站数组（sing-box outbound） |
| `$content` | 脚本写回的最终配置文本 |

脚本在 subLinkPro 的 Goja 运行时里执行，输出与 Sub-Store 使用同一份脚本时**逐字节一致**（含 `JSON.stringify` 缩进）。

### 使用方式

1. 模板：在模板页新增 `singbox` 类别的 JSON 模板（例如 `sing-box-1.12.base.json`）。
2. 脚本：在脚本页新增脚本，内容用 Sub-Store Script Operator 写法；参数写在脚本的 `args` 字段里（JSON，例如 `{"profile":"company","tailscale":"true,sing-box"}`）。
3. 订阅：给订阅挂上这个脚本，并把订阅 `config` 的 `singbox` 指向模板路径。

请求 `?client=sing-box` 时，subLinkPro 先把节点转成 sing-box 出站，再交给脚本渲染；脚本不存在或执行失败时退回模板直接合并。

### 节点顺序

节点在配置里的顺序取自上游订阅顺序（`source_sort`），与 Sub-Store 读取同一份订阅时的顺序一致；未记录上游顺序的节点排在后面并保持原有 id 顺序。
