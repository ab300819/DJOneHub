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

## 已知的后续缺口

- **进度条 UI 无处安放**：事件通道已通，但 SwiftUI App 还没有 eSIM 页面。
  网页控制台是 5 视图（overview / network / sms / esim / at），SwiftUI 只有 3 页，
  缺 network 和 esim，且 overview 没有"联网活动"。
- **DTO 手抄两次**：Swift 侧已手写一遍，Android 还要再写。可考虑从 Go 结构体
  的 `json` tag 生成两侧模型，但建议等 Android DTO 真写起来、痛感具体了再决定。
