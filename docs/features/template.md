# Configuration templates

A template is the skeleton file of a subscription output. Nodes and proxy groups are injected when the subscription is rendered.

## Categories

| Category | Typical file | Purpose |
| --- | --- | --- |
| `clash` | `clash.yaml` | Clash / mihomo / Stash configuration |
| `surge` | `surge.conf` | Surge configuration |
| `singbox` | `singbox.json` | sing-box configuration |

The category is inferred from the extension: `.conf` → `surge`, `.json` → `singbox`, anything else → `clash`. Template files live in `template/` under the working directory and are imported into the database on startup.

## sing-box templates

When a subscription is requested with `?client=sing-box` (or `singbox`), the server renders a **complete configuration** from the sing-box template instead of relying on Sub-Store conversion.

The template is a regular sing-box JSON configuration. Merge rules:

1. **Template `outbounds`** are kept as-is (`direct`, `block`, selectors, ...). The `__ALL_PROXIES__` placeholder inside an `outbounds` array expands to every node.
2. **`proxy-groups` from the Clash template** become sing-box outbounds: `select` → `selector`, `url-test` / `fallback` / `load-balance` → `urltest` (`url`, `interval`, `tolerance` carried over). `DIRECT` members are rewritten to the template's `direct` tag, `REJECT` members to its `block` tag (dropped when the template has no `block` outbound).
3. **Node outbounds** are appended last, tagged with the node name.

On tag conflicts the template wins. `route`, `dns`, `inbounds` and `experimental` are entirely template-defined.

### Supported node protocols

`ss` (including obfs-local / v2ray-plugin), `vmess`, `vless` (REALITY, ws / grpc / h2 / httpupgrade), `trojan`, `hysteria2` (including port hopping), `tuic`, `anytls`, `socks5`, `http`. Other protocols (ssr, snell, mieru, wireguard, openvpn) are skipped and logged.

### Subscription configuration

Add a `singbox` key to the subscription `config` JSON:

```json
{"clash":"./template/clash.yaml","surge":"./template/surge.conf","singbox":"./template/singbox.json","udp":false,"cert":false}
```

Without it, `template/singbox.json` in the working directory is used. If neither exists, the request falls back to the Sub-Store sidecar, which only returns the node `outbounds` list.

### Built-in template

`template/singbox.json` is a working starting point: mixed inbound on 7890, a `节点选择` selector (all nodes), split DNS with direct/proxy upstreams, `hijack-dns` and private-address direct rules. Typical edits: switch to a `tun` inbound, change DNS upstreams, add `rule_set` routing.

## sing-box render scripts

Besides the direct template + node merge, a subscription can carry a **Sub-Store Script Operator style** render script that produces the final configuration.
Such a script ships its own `Runtime` / `ProxySource` / `Pipeline` and talks to the host through four contract variables:

| Variable | Meaning |
| --- | --- |
| `$arguments` | Script arguments (JSON object), taken from the script `args` field |
| `$files` | Template file array; `$files[0]` is the template the subscription `config.singbox` points to |
| `produceArtifact` | Host provided function returning the subscription nodes as sing-box outbounds |
| `$content` | Final configuration text written back by the script |

The script runs in subLinkPro's Goja runtime and the output is **byte-identical** to Sub-Store running the same script (including `JSON.stringify` indentation).

### How to use

1. Template: add a JSON template with the `singbox` category (for example `sing-box-1.12.base.json`).
2. Script: add a script written in the Sub-Store Script Operator style; put the arguments in the script `args` field (JSON, for example `{"profile":"company","tailscale":"true,sing-box"}`).
3. Subscription: attach that script and point the subscription `config` `singbox` key at the template.

For `?client=sing-box` requests subLinkPro converts the nodes to sing-box outbounds and hands them to the script; when no script is attached or the script fails, the template merge path is used instead.

### Node order

Nodes are emitted in upstream subscription order (`source_sort`), matching the order Sub-Store sees for the same source; nodes without a recorded upstream order come last and keep their id order.
