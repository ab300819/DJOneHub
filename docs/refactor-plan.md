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

## 待做

### 第二趟：依赖倒置（两个接口）

`main.go` 里约 110 个函数，其中一大块是 macOS 主机探测，而 5 个 service 方法
直接依赖它们。要抽两个接口而非一个：

| 接口 | 职责 | macOS 实现 | Android 实现 |
| --- | --- | --- | --- |
| `ATTransport` | 与模块通信 | libusb / cgo | Kotlin `bulkTransfer` 经 gomobile 回调 |
| `HostProbe` | 探测主机 OS | `ioreg` / `ifconfig` / `route` / `nettop` | Android API，或明确返回不支持 |

`ATTransport` 对外只需 4 个方法（`Command` / `CommandWithPrompt` / `Close` /
`Description`），且已有 `//go:build darwin && cgo` 的平台分割，接口是现成的形状。

`HostProbe` 覆盖四类探测，牵动 `NetworkDiagnostic` / `NetworkTraffic` /
`Check4GRoute` / `LocalNetworkConnection` / `NetworkActivity`。

倒置后核心逻辑可脱离硬件测试——目前做不到。

### 第三趟：搬包

依赖方向正确之后位置才没有歧义：

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

覆盖 21 个端点响应体 + 5 个错误状态码。基线用 `git worktree` 单独构建，
不受工作区状态影响。

## 已知的后续缺口

- **进度条 UI 无处安放**：事件通道已通，但 SwiftUI App 还没有 eSIM 页面。
  网页控制台是 5 视图（overview / network / sms / esim / at），SwiftUI 只有 3 页，
  缺 network 和 esim，且 overview 没有"联网活动"。
- **DTO 手抄两次**：Swift 侧已手写一遍，Android 还要再写。可考虑从 Go 结构体
  的 `json` tag 生成两侧模型，但建议等 Android DTO 真写起来、痛感具体了再决定。
