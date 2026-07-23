# ShareWiFi

ShareWiFi 是一个面向 Linux 的 Wi-Fi 热点共享管理程序。它以单一 Go 二进制运行，内嵌中文 Web 控制台，用于配置和管理基于 `hostapd`、`dnsmasq` 的 WPA2 无线热点，并通过 IPv4 NAT 共享上游网络。

当前为第一阶段实现，重点是单热点、IPv4 NAT、可观察的启动错误和基础客户端流量监控。

## 当前功能

- 启动后提供 Web 控制台，默认监听 `0.0.0.0:8080`。
- 启动时检查 root 权限与所需系统命令，并在页面展示 Debian/Ubuntu、Fedora/CentOS 的安装提示。
- 选择无线网卡，配置 SSID、WPA2 密码、国家代码、频段、信道、网关与 DHCP 地址池。
- 自动探测默认路由上游接口；也可显式指定上游网卡。
- 使用 `hostapd` 创建 AP、使用 `dnsmasq` 提供 DHCP。
- 自动优先使用 `nftables`，否则使用 `iptables` 配置 IPv4 转发与 NAT。
- AP 网卡由 NetworkManager 管理时，启动期间临时设为 unmanaged；正常停止时恢复管理。
- 网页可导入、导出热点 JSON 配置；`--config` 可直接启动热点。
- `hostapd`、`dnsmasq` 日志保存在运行目录。启动失败时，页面和服务端日志会显示原因。
- 可按需查看客户端 MAC、DHCP 主机名、IP、信号、协商速率、累计流量和近似实时速率。
- 可按需显示热点总上传/下载速率图表。监控区折叠时不会调用 `hostapd_cli` 或读取流量计数。

## 运行环境

支持目标为 Debian/Ubuntu，以及 Fedora/CentOS/RHEL 系列 Linux。程序本身只支持 Linux，且必须由 root 启动：

```sh
sudo ./sharewifi
```

运行时依赖：

| 用途 | 命令 | 是否必需 |
| --- | --- | --- |
| 创建 AP | `hostapd` | 是 |
| DHCP | `dnsmasq` | 是 |
| 网络地址和路由管理 | `ip` | 是 |
| 无线网卡与 AP 能力检测 | `iw` | 是 |
| NAT | `nft` 或 `iptables` | 至少一个 |
| 读取客户端状态 | `hostapd_cli` | 否，通常随 `hostapd` 一起安装 |
| NetworkManager 协作 | `nmcli` | 否，检测到时使用 |

安装示例：

```sh
# Debian / Ubuntu
sudo apt update
sudo apt install hostapd dnsmasq iproute2 iw nftables

# Fedora / CentOS / RHEL
sudo dnf install hostapd dnsmasq iproute iw nftables
```

无线网卡和驱动必须支持 AP mode。可用 `iw list` 检查 `Supported interface modes` 中是否包含 `* AP`。

## 使用方法

### Web 控制台

```sh
sudo ./sharewifi
sudo ./sharewifi --listen 127.0.0.1:8080
sudo ./sharewifi --workdir /var/lib/sharewifi
```

启动后访问 `http://主机地址:8080`。默认页面已提供一组可编辑的热点与 DHCP 参数。

### 命令执行日志

默认只输出服务启动、停止和错误日志。需要排查热点创建、DHCP、NetworkManager 或防火墙规则时，使用 `-info` 输出每个外部命令的用途、完整参数，以及 `nft` 规则文本：

```sh
sudo ./sharewifi -info
```

该开关不会改变热点行为，仅增加控制台诊断输出。

### 控制台认证

可选的 HTTP Basic Auth 认证需要用户名和密码同时提供：

```sh
sudo ./sharewifi --username admin --password 'replace-this-password'
```

用户名和密码都不提供时不启用认证。Basic Auth 不加密传输内容；默认监听所有地址且不启用 TLS，因此不应暴露在不可信网络或公网。建议至少设置认证，或以 `--listen 127.0.0.1:8080` 配合 SSH 隧道、反向代理 TLS 使用。

### 从配置直接启动

网页“导出配置”生成的是热点配置 JSON，其中包含 Wi-Fi 密码明文，应按敏感文件保护。

```sh
sudo ./sharewifi --config /etc/sharewifi/home.json
sudo ./sharewifi --config /etc/sharewifi/home.json \
  --listen 127.0.0.1:8080 \
  --workdir /var/lib/sharewifi \
  --username admin --password 'replace-this-password'

# Web 控制台立即启动，30 秒后按 JSON 创建热点
sudo ./sharewifi --config /etc/sharewifi/home.json --delay 30

sudo ./sharewifi -listen 0.0.0.0:8081 -username zoulei -password admin123 -workdir /home/zouleid/tmp -config /home/zouleid/tmp/sharewifi.json
```

`--config` 仅读取热点和 DHCP 参数；`--listen`、`--workdir`、`--username`、`--password`、`-info`、`--delay` 是进程级参数，不会写入 JSON，可以与 `--config` 同时使用。`--delay` 的单位是秒，默认 `0`，仅在同时指定 `--config` 时生效；延迟期间 Web 控制台可正常访问，正常退出程序会取消尚未开始的热点创建。

配置示例：

```json
{
  "interface": "wlan0",
  "ssid": "ShareWiFi",
  "passphrase": "change-me-123",
  "country_code": "CN",
  "band": "2.4GHz",
  "channel": 6,
  "gateway_cidr": "192.168.50.1/24",
  "dhcp_start": "192.168.50.20",
  "dhcp_end": "192.168.50.200",
  "lease_time": "12h",
  "upstream_interface": "",
  "allow_upstream_lan": false,
  "upstream_lan_cidr": ""
}
```

`upstream_interface` 为空时使用系统默认 IPv4 路由对应的接口。

`allow_upstream_lan` 默认关闭。启用后，程序仅允许 `upstream_lan_cidr` 指定的上游 IPv4 网段主动访问热点网段。例如主机上游地址为 `172.16.41.50/24`、热点网段为 `192.168.50.0/24` 时，填写 `172.16.41.0/24`。上游设备还必须具有到热点网段的路由，例如在 `172.16.41.40` 上执行：`sudo ip route replace 192.168.50.0/24 via 172.16.41.50`。

## 设计与运行机制

1. Web 表单由后端校验后，在运行目录生成 `hostapd.conf`、`dnsmasq.conf`。
2. 程序给 AP 网卡配置网关地址，启用 `net.ipv4.ip_forward`，再创建 NAT 和转发规则。
3. `hostapd` 以前台子进程方式运行并创建控制 socket；`dnsmasq` 仅提供 DHCP，设置 `port=0`，避免与系统 DNS 服务争用 53 端口。
4. DHCP 下发从 `/etc/resolv.conf` 发现的非 loopback DNS；未发现时使用 `223.5.5.5`（阿里）与 `114.114.114.114`（114DNS）。
5. `dnsmasq.leases` 保存在运行目录，用于将 `hostapd_cli` 站点 MAC 关联为 IP 和 DHCP 主机名。
6. 停止时终止 `dnsmasq`/`hostapd`、删除本程序的防火墙规则、恢复原有 IPv4 转发值和 NetworkManager 管理状态。

运行目录未指定时在系统临时目录创建；指定 `--workdir` 有利于保留配置、租约和日志以便排障。该目录中的 `hostapd.conf` 和导出的 JSON 都含有明文 Wi-Fi 密码，应限制访问权限。

## 监控说明

“已连接设备与流量”区域默认折叠。展开后前端才会请求 `/api/clients`，后端调用 `hostapd_cli all_sta` 并读取热点网卡字节计数；收起后会停止请求。速率由两次请求之间的累计字节差计算，因此首次采样显示 `0 B/s`，后续约每 3 秒更新一次。

信号和协商速率依赖无线驱动及 hostapd 输出，部分设备可能显示为空。DHCP 主机名由客户端上报，可能为空或不准确。

## 注意事项与限制

- 创建热点前会对 AP 网卡执行 `ip addr flush`。不要将当前管理连接依赖在这张网卡上，否则会断网。
- 本程序只管理 IPv4 NAT，不支持桥接、IPv6 转发、开放网络、WPA Enterprise、多 AP 实例或带宽限速。
- `nftables` 使用专属 `table ip sharewifi`；`iptables` 只添加并删除精确规则。若其他防火墙工具（如 firewalld、ufw）阻断转发，仍需按系统策略额外处理。
- 正常使用页面“停止”、`Ctrl+C` 或 `SIGTERM` 退出会执行清理。`kill -9`、崩溃或意外断电可能遗留 hostapd、dnsmasq、接口地址和防火墙规则。
- 当前第一阶段没有持久化运行状态和遗留会话接管能力。异常退出后重新启动的程序不能自动识别并停止旧热点；此时请先人工检查并清理遗留进程与规则。
- 程序不会自动安装系统包，也不会绕过地区无线监管限制。实际可用信道由国家代码、网卡和驱动决定。

## 编译

项目仅使用 Go 标准库，`web.html` 通过 `go:embed` 编译进二进制。建议使用 Go 1.22 或更新版本。

### 本机构建

```sh
go build -o sharewifi main.go
go vet main.go
```

### Linux 交叉编译

程序运行时仅支持 Linux，因此交叉编译的目标应为 Linux。禁用 CGO 可得到不依赖目标 C 运行库的静态 Go 二进制：

```sh
# x86_64 Linux
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/sharewifi-linux-amd64 main.go

# ARM64 Linux，例如多数 ARM 开发板
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o dist/sharewifi-linux-arm64 main.go

# 32 位 ARM Linux
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -o dist/sharewifi-linux-armv7 main.go
```

交叉编译只解决二进制架构问题；目标机仍需安装对应架构的 `hostapd`、`dnsmasq`、`iproute`、`iw` 和防火墙工具，并具备支持 AP 的无线驱动。

### 缩小二进制

发布构建可移除调试符号、路径信息和 VCS 元数据：

```sh
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -ldflags='-s -w' \
  -o dist/sharewifi-linux-amd64 main.go
```

`-s -w` 会降低二进制体积，但会减少 `panic`/调试信息。可选使用 UPX 进一步压缩：

```sh
upx --best --lzma dist/sharewifi-linux-amd64
```

UPX 会增加启动时解压开销，并可能不符合部分安全软件、发行政策或调试流程；发布前应在目标发行版验证。不要压缩仍需用调试器排查的问题版本。

## 仓库结构

```text
main.go       Go 服务、网络配置、进程和 API
web.html      嵌入二进制的中文 Web 控制台
README.md     项目现状、运行与维护说明
.gitignore    本地产物和运行文件忽略规则
```
