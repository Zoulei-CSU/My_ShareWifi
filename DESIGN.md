# ShareWiFi 设计与维护说明

本文档面向维护者，记录当前实现的架构、运行机制、限制、排障注意事项和编译方法。面向安装与使用的说明见 [README.md](README.md)。

## 设计目标

- 单一 Go 二进制，Web 页面通过 `go:embed` 内嵌，不依赖运行时静态资源服务器。
- 使用发行版常见的 `hostapd`、`dnsmasq`/`udhcpd`、`iproute2` 和防火墙工具，避免自行实现 802.11 AP、DHCP 或 NAT。
- Web 只管理热点参数；监听地址、运行目录和控制台认证属于服务进程参数，不进入热点 JSON 文件。
- 在启动失败时保留有用的进程日志，并在停止或启动回滚时尽量恢复主机网络状态。

## 架构

```text
浏览器
  | HTTP / Basic Auth（可选）
ShareWiFi（main.go，net/http）
  |-- Web/API 与 JSON 配置校验
  |-- hostapd：创建 WPA2 AP 与控制 socket
  |-- dnsmasq 或 udhcpd：DHCP 服务
  |-- ip / sysctl：接口地址、链路状态、IPv4 转发
  |-- nftables 或 iptables：转发与 NAT
  `-- nmcli：临时解除和恢复 AP 网卡的 NetworkManager 管理
```

当前源码没有第三方 Go 依赖。`main.go` 负责 HTTP、配置校验、进程与网络生命周期；`web.html` 为嵌入页面；`img/` 只用于仓库文档。

## 配置模型

热点 JSON 包含无线、DHCP、上游接口以及允许上游 LAN 访问的选项。Wi-Fi 密码以明文存储。

下列参数只保存在进程内存，必须通过命令行提供，且 JSON 导入、导出和 Web API 均不会暴露它们：

- `-listen`
- `-workdir`
- `-username`
- `-password`
- `-info`
- `-delay`

`-config` 可与上述进程级参数组合使用。`-delay` 仅作用于 `-config` 触发的自动启动；延迟启动前会先解析 JSON，并在 Web 表单中显示其热点参数。

## 热点启动与停止生命周期

启动顺序：

1. 校验网卡、SSID、密码、国家代码、频段/信道、热点网段、DHCP 地址范围以及可选上游 LAN CIDR。
2. 探测上游接口；未指定时读取系统默认 IPv4 路由。
3. 若存在 `nmcli`，记录 AP 网卡的 NetworkManager 状态并临时设为 unmanaged。
4. 清除 AP 网卡地址，配置热点网关地址并拉起网卡。
5. 保存此前的 `net.ipv4.ip_forward` 值并启用 IPv4 转发。
6. 创建专属 `nftables` 表，或添加精确的 `iptables` 转发和 NAT 规则。
7. 在运行目录生成 `hostapd.conf` 与当前 DHCP 后端的配置文件，依次以前台子进程方式启动。
8. 启动后检查子进程是否存活；失败时执行反向清理并保留日志。

页面通过 `iw phy ... info` 获取网卡支持的 2.4GHz/5GHz 信道，读取失败时回退到 `iw list`；后端启动时也会再次校验，不能依赖浏览器校验绕过能力限制。界面中的频段和手动信道列表保留该网卡报告的全部硬件频率，不会因启动前监管域暂时标记 `disabled` 而错误隐藏 5GHz；最终能否创建 AP 仍由 `hostapd`、国家代码、DFS 与驱动决定。若两种读取方式均失败或未能解析信道，系统仅给出警告，并回退到内置固定 2.4GHz/5GHz 信道表；此时禁用自动信道，必须手动选择信道。选择自动信道（JSON 中 `channel` 为 `0`）时，程序在启动 AP 前执行 `iw dev <interface> scan`，优先从当前监管状态允许主动发射的信道中统计 BSS 数量并选择占用较少的候选项；若没有此类候选项，则回退到硬件报告的信道。扫描失败时按 2.4GHz 的 1/6/11 或 5GHz 的 36/40/44/48 等首选顺序择一。扫描结果受网卡驱动、接口状态、国家代码和 DFS 限制影响，自动选择不是无线环境的绝对最优解。

停止、`SIGINT`、`SIGTERM` 或启动失败回滚时，会终止 DHCP 服务与 `hostapd`、清理防火墙规则、恢复 IPv4 转发值，并恢复 NetworkManager 对 AP 网卡的管理。

`kill -9`、崩溃和意外断电无法触发清理。当前第一阶段没有运行状态持久化或遗留会话接管能力；异常退出后，新进程不能自动接管和停止旧热点，应人工检查遗留的进程、接口地址和防火墙规则。

## 网络与防火墙

默认热点模式是 IPv4 NAT：客户端可访问上游网络，上游对客户端的新连接默认不放行。

启用 `allow_upstream_lan` 时，程序额外放行：

```text
upstream_lan_cidr -> hotspot subnet
```

规则同时限制来源 CIDR、目标热点网段、上游接口与 AP 接口。上游主机仍须配置到热点网段的路由；这不是 NAT 规则能够替代的。

优先使用专属 `table ip sharewifi` 的 `nftables` 规则；即使系统同时安装 `nft` 和 `iptables`，也始终选择 `nftables`。仅系统没有 `nft` 时才使用 `iptables`。`firewalld`、`ufw` 或其他防火墙工具仍可能覆盖或阻断转发，需按目标系统策略处理。

## DHCP、DNS 与客户端监控

DHCP 后端按如下顺序选择：存在 `dnsmasq` 时使用 `dnsmasq`；仅当 `dnsmasq` 不存在且 `udhcpd` 存在时才使用 `udhcpd`；两者都不存在则拒绝启动。`dnsmasq` 配置为 DHCP-only（`port=0`），避免争用系统 DNS 服务的 53 端口。DHCP 下发 `/etc/resolv.conf` 中检测到的非 loopback DNS；无法发现时使用 `223.5.5.5`（阿里 DNS）和 `114.114.114.114`（114DNS）。

`dnsmasq` 的 `dnsmasq.leases` 保存在运行目录。客户端监控通过 `hostapd_cli all_sta` 获取站点 MAC 和字节计数，再关联该租约文件获取 IP 与 DHCP 主机名；热点网卡总流量读取网卡字节计数。`udhcpd` 的租约文件为二进制格式，当前不解析，因此使用 `udhcpd` 时客户端列表仍显示 MAC、信号和流量，但 IP 与 DHCP 主机名可能为空。前端仅在展开“已连接设备与流量”区域或点击刷新后调用 API；收起后不进行这类采集。速率由两次请求间的字节差计算，首次采样显示 `0 B/s`。

信号、协商速率、主机名均依赖网卡驱动、hostapd 输出和客户端上报，可能为空或不准确。

## 日志与排障

运行目录包含：

| 文件 | 用途 |
| --- | --- |
| `hostapd.conf` | 生成的热点配置，含 Wi-Fi 密码。 |
| `dnsmasq.conf` 或 `udhcpd.conf` | 当前 DHCP 后端生成的配置。 |
| `hostapd.log` | hostapd 标准输出与错误输出。 |
| `dnsmasq.log` 或 `udhcpd.log` | 当前 DHCP 后端的标准输出与错误输出。 |
| `dnsmasq.leases` 或 `udhcpd.leases` | 本实例 DHCP 租约；后者为二进制格式。 |

页面会显示日志尾部，启动 API 的失败响应也会包含错误信息。排查网络命令、NetworkManager 和防火墙规则时，使用 `-info`；该开关会向控制台输出每个外部命令的用途、完整参数及 `nft` 标准输入规则。

## 注意事项与限制

- 程序必须以 root 启动。
- 创建热点会对选中的 AP 网卡执行 `ip addr flush`。不要选择当前 SSH、桌面会话或远程管理正在使用的网卡。
- Web 控制台默认监听所有地址且不启用 TLS。未提供用户名和密码时无鉴权；不要暴露给不可信网络或公网。建议设置认证、限制监听地址、使用防火墙或经 TLS 反向代理访问。
- HTTP Basic Auth 只提供认证，不提供传输加密；使用 `http://` 时用户名和密码会在网络上以可解码形式传输。
- 运行目录、`hostapd.conf` 和导出的 JSON 含有明文 Wi-Fi 密码。应限制目录与文件访问权限，例如 `0700`/`0600`。
- 仅支持 IPv4 NAT；不支持桥接、IPv6 转发、开放热点、WPA Enterprise、多 AP 实例或带宽限速。
- 实际可用频段和信道受国家代码、无线网卡、驱动及地区监管限制。程序不会绕过这些限制。
- 程序不会自动安装系统软件包。

## 编译

项目仅使用 Go 标准库，建议 Go 1.22 或更新版本。

### 本机构建

```sh
go build -o sharewifi main.go
go vet main.go
```

### Linux 交叉编译

程序运行时仅支持 Linux。禁用 CGO 可生成不依赖目标 C 运行库的 Go 二进制：

```sh
# x86_64 Linux
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/sharewifi-linux-amd64 main.go

# ARM64 Linux，例如多数 ARM 开发板
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/sharewifi-linux-arm64 main.go

# 32 位 ARM Linux
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -o dist/sharewifi-linux-armv7 main.go
```

交叉编译只解决二进制架构问题。目标机器仍须安装对应架构的 `hostapd`、`dnsmasq` 或 `udhcpd`、`iproute`、`iw` 和至少一种防火墙工具，并具备支持 AP 模式的无线驱动。

### 缩小发布二进制

发布构建可移除调试符号、路径信息和 VCS 元数据：

```sh
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -ldflags='-s -w' \
  -o dist/sharewifi-linux-amd64 main.go
```

`-s -w` 会减小二进制，但也会减少 `panic` 与调试信息。可选使用 UPX 进一步压缩：

```sh
upx --best --lzma dist/sharewifi-linux-amd64
```

UPX 会增加启动时解压开销，并可能不符合部分安全软件、发行政策或调试流程；发布前应在目标发行版验证。不要压缩仍需使用调试器排查的问题版本。
