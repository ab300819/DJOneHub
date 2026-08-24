# 多产物重构方案

目标：一份核心，两个产物。

- **B/S 产物** `cmd/djonehub`：HTTP 服务 + 内嵌网页控制台（现行方式）
- **C/S 产物**：核心编成库，供原生 App 调用
  - Android：`gomobile bind` → AAR（纯 Go，无 cgo）
  - macOS：`c-archive` → `.a` + `.h`（可选，见下）

## 已完成

**统一入口（commit b882cf2）**

```
              Call(svc, requestJSON) → responseJSON
                            ▲
        ┌───────────────────┼────────────────────┐
   stdio 循环            c-archive 导出       gomobile 导出
   （已有）                 （待建）             （待建）
```

`Call` 是每个传输层唯一需要知道的契约。响应带 `kind` 字段承载错误分类，
客户端不必匹配错误文本。事件通道（`EventSink`）已就位，eSIM 下载进度可推送；
不能推送的传输不装 sink，行为与之前一致。

**`ATTransport`**

核心对模块的唯一视图，4 个方法。`openDJIUSBAT` 返回接口而非 `*usbAT`——
存进接口字段的 nil 指针不是 nil 接口，而 `a.usbAT != nil` 在十几处被判断。
AT 响应解析（`atResponseComplete` / `atProbeSucceeded` 等）是纯文本处理，
一并搬到接口这一侧，`usbat_stub.go` 补上 `Description()` 后非 darwin 构建
首次编译通过——之前一直是坏的，CI 只跑 macOS 所以没暴露。

收益立刻兑现：`attransport_test.go` 用假实现覆盖了 `usbATStatus` 的 11 条
AT 命令解析、掉线时丢弃句柄的生命周期、以及短信分段提交——全都不再需要硬件。

**`HostProbe`**

五个方法覆盖 `ioreg` / `ifconfig` / `route -n get default` / `netstat -ibn` /
`nettop`，注入到 `app.host`。核心里不再出现任何命令行工具的名字，
`NetworkDiagnostic` / `NetworkTraffic` / `Check4GRoute` /
`LocalNetworkConnection` / `NetworkActivity` 五个方法全部改为向接口提问。

不支持的平台由 `unsupportedHost` 承担——全部返回空而不是报错，形状与 macOS
上"没插模块"一致。它不带构建标签，因为"没有平台能力"本身不是平台特定的，
这样测试能直接跑真实实现而不是抄一份。

两个接口分两个提交做，不是一次抽完：出问题时能定位到是哪个接口引入的。

`hostprobe_test.go` 用假主机描述机器状态，覆盖了默认出口是模块 / 是 Wi-Fi /
读不到三种判定、流量基线首采样与模块重启后计数回退、VPN 起来时连接列表要从
隧道而不是模块网卡上读、采样器失败不能带走整个快照。这些分支此前只能靠开发机
当时恰好插着什么来碰。

## 顺带修掉的一个真实错误

`selectUSBTrafficInterface` / `hasLikelyUSBNetworkInterface` / `Check4GRoute`
三处判据都是"`en0` 是 Wi-Fi，其余活跃的 `en*` 就是模块"。macOS 的接口名按枚举
顺序分配，跟硬件无关：开发机上 `en0` 是有线、`en1` 是 Wi-Fi，于是
`/api/network/traffic` 把 74 GB 的 Wi-Fi 流量当成 4G 模块流量报了出来，
`usb_network_present` 在没插模块时也是 true。

正解是从模块自己反查：`ioreg -r -c IOUSBHostDevice -l` 每个 USB 设备一个
空行分隔的块，块内含整棵子树，所以 ECM 驱动的 `BSD Name` 就在模块 `idVendor`
所在的那个块里。`HostProbe.ModuleInterface()` 返回这个名字，判据全部改为
"默认出口是不是模块的网卡"，查不到就报告没有，不再兜底挑别的网卡。

护栏如实报出两处刻意变化：`usb_network_present` true→false，
`network/traffic` 由 `en1` 变为不可用。`check-4g` 无变化只因为当时默认出口在
VPN 隧道上，旧代码恰好也返回否；测试补上了默认出口落在非模块网卡的场景。

**已用真实硬件确认**（2026-08-24）。模块的 IOKit 链路是
`CDC Ethernet Control Model (ECM)@4` → `AppleUserECM` →
`IOSkywalkLegacyEthernet` → `en11`，`"BSD Name"` 在整份
`ioreg -r -c IOUSBHostDevice -l` 输出里只出现一次，就在模块块内。测试样本已换成
真实输出的裁剪版。

修复前后在模块在位的条件下对比，三处视图同时从 Wi-Fi 纠正到模块网卡：

| 端点 | 修复前 | 修复后 |
| --- | --- | --- |
| `network/local` | `en1` / `10.10.51.59` | `en11` / `192.168.225.23` |
| `network/traffic` | `en1` | `en11` |
| `network/activity` | `en1` / `10.10.51.59` | `en11` / `192.168.225.23` |

`ATTransport` 同时用真实硬件验证：`Description()` 返回
`USB AT · 2c7c:0125 interface 2 out 0x03 in 0x84`，11 条 AT 状态读取拿到中国电信
LTE 的真实值，`ping -b en11 8.8.8.8` 零丢包——4G 出网通路成立。

## 待做

### 第三趟：搬包

依赖方向已经正确，位置就没有歧义了。两个接口的实现函数目前还留在 `main.go`
里（`discoverMac*` 一族、`parseNettopActivity`、libusb 那套），这一趟把它们搬走：

```
core/              可导入的库包（AT 协议 / SMS / eSIM / service 实现 / dispatch）
platform/darwin/   libusb + ioreg/ifconfig/nettop 实现
cmd/djonehub/      B/S：HTTP + 内嵌 web UI
mobile/            C/S：gomobile bind 入口
clib/              C/S：c-archive 入口（package main + //export）
```

### 第四趟：Android

本机 gomobile、Android SDK/NDK **均未安装**，开工前需先装工具链。
Android App 作为第三个子模块 `DJOneHubDroid`（Kotlin / Gradle / Compose）。

## 平台差异不是妥协

macOS 保留子进程 + 管道，Android 只能链库——因为 iOS/Android 不允许派生子进程，
而 macOS 可以，且进程隔离有实际价值（核心崩溃不会带走 App）。两者走同一个
`Call`、同一份 JSON 契约，App 层代码结构相同，只有语言语法不同。

macOS 若将来想改单进程，是链接期开关，不是重写。

## 重构护栏

每趟改动都用 `scripts/compare-api.sh` 验证 HTTP 行为不变：

```sh
scripts/compare-api.sh baseline <重构前的提交>
make build
scripts/compare-api.sh after
scripts/compare-api.sh diff
```

覆盖 21 个端点响应体 + 5 个错误状态码，端点消失也算不一致。有漂移则退出码非 0，
可以直接串在命令里。基线用 `git worktree` 单独构建，不受工作区状态影响。

护栏本身踩过三个坑，都已修掉——它给出的"一致"必须是可信的，否则整套重构的
验证都是假的：

- `_codes.txt` 只采集、从不比对，"5 个错误码"的覆盖曾经是空话
- 一个慢端点（`nettop` 那 30 秒）让 `set -eu` 中断采集，留下残缺基线，
  而 diff 只遍历"存在的文件"，于是残缺基线**静默通过**。现在采集写
  `_manifest.txt` 记录意图采集的全部端点，diff 校验两侧齐全且非空
- 采集中断时 `kill` 没执行，端口上留下孤儿 demo 服务，之后每次采集都在跟
  那个陈旧进程说话，读出来像是行为差异。现在端口被占直接拒绝启动，并用
  `trap` 保证中断也清理

## 已知的后续缺口

- **进度条 UI 无处安放**：事件通道已通，但 SwiftUI App 还没有 eSIM 页面。
  网页控制台是 5 视图（overview / network / sms / esim / at），SwiftUI 只有 3 页，
  缺 network 和 esim，且 overview 没有"联网活动"。
- **DTO 手抄两次**：Swift 侧已手写一遍，Android 还要再写。可考虑从 Go 结构体
  的 `json` tag 生成两侧模型，但建议等 Android DTO 真写起来、痛感具体了再决定。
