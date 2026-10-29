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
