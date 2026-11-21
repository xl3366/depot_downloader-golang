# DepotDownloader-Go

基于 Golang 编写的极速、高并发、跨平台 Steam Depot 下载工具，专门针对多 Manifest 大文件下载场景进行了深度架构重构与性能优化。

---

## 核心特性与架构优势

### 1. 彻底解决 Python 版长时间大文件下载的断流与 Token 痛点
* **长连接原生保活与故障自愈（Auto-Reconnect & Heartbeat）**：
  基于原生 WebSocket（`wss://`）长连接构建，内置心跳协程（每 30 秒自动发送 `EMsgClientHeartBeat`）。如遇长时间大文件下载导致网络波动或服务器主动断开，客户端透明重连并重新匿名登录，彻底杜绝连接假死。
* **智能 Token 生命周期管理与 401/403 自动刷新**：
  以 `(app_id, depot_id, cdn_host)` 为维度独立维护 CDN Auth Token 与 TTL。在 Token 即将过期前 2 分钟，或任何下载协程遇到 HTTP 401/403 授权失效时，自动触发重新申请并无缝重试该 chunk，多 Depot 轮换下载绝不报错。

### 2. 纯物理级断点续传（无需依赖进度 JSON 碎片）
* **与 Python 版的区别**：
  Python 版在下载时会在当前目录生成散落的进度切片文件（如 `{depot_id} - xx%.json`），记录已下分块的 Hash；若该 JSON 丢失或损坏，Python 就会强制从头重新全量下载。
* **Go 版物理级续传机制**：
  **分块断点续传完全不依赖任何外部进度 JSON 文件**。无论下载因断电、断网或手动中断，下次启动时下载器会直接就地读取已有文件已写入的每个 chunk 数据，计算 Steam 原生的 `Adler32 (seed=0)` 进行就地校验。校验一致的 chunk 自动秒过，只拉取缺失或未写完整的 chunk，极度抗崩溃。

### 3. 标准化增量更新（Manifest-Level Incremental Updates）
* 遵循 Steam / DepotDownloader 官方标准，在目标输出目录下维护 `.DepotDownloader/depots.json`（记录当前已安装的清单 GID）：
  * **未变更文件**：新旧 Manifest 中 `sha_content` 与大小一致的文件，**零网络与磁盘 I/O 直接跳过**；
  * **局部变动文件**：自动比对 chunk 差异，未变动分块原地复用，仅下载有改动分块并通过 `WriteAt` 原地覆写更新；
  * **废弃清理**：支持 `-d, --delete` 自动清理新清单中已移除的陈旧文件。

### 4. 协议灵活与国内网络优化
* **支持纯 HTTP 模式（`--use-http`）**：
  将 WebAPI 接口与 CDN 下载降级为纯 `http://`，免去密集高并发 TLS 握手开销，大幅降低 CPU 占用，同时规避国内某些网络环境下对 HTTPS SNI 的阻断限速。
* **区域智能调度与容错**：
  支持 `-u China` / `api.steamchina.com` 专线，内置国内 CDN（电信天翼云、白山云、网宿等）自动优选与自适应降级，避开专网 403 节点。

### 5. 高性能多协程就地写入
* 真正的多协程高并发下载（默认 32 协程，可自定），结合 `os.File.WriteAt` 实现同一大文件多 offset 无锁并发就地写入。
* 完整支持 Steam 主流压缩格式：`VSZa` (VZstd / zstd)、`VZa` (VZip / LZMA)、`Zip`。
* 终端平滑进度条显示：实时百分比、实时传输速率 (MB/s)、已下载/总数据量。

---

## 编译与安装

项目支持跨平台编译（Windows / Linux / macOS）：

```powershell
# 在 depotdownloader-go 目录下执行构建
go build -o depotdownloader.exe ./cmd/depotdownloader
```

---

## 常见使用场景与示例

### 1. 批量下载目录或 .zip 压缩包（如 HubcapManifest 导出的应用包）
```powershell
# 直接传入 .zip 压缩包或解压目录（支持组合参数如 -lu China，无需解压直接拉取）
.\depotdownloader.exe -lu China 813230.zip

# 指定输出目录与完整性校验（-i）
.\depotdownloader.exe -lu China -p ./813230.zip -o ./my_game -i

# 自定义 64 协程加速下载
.\depotdownloader.exe -lu China -t 64 ./813230.zip

# 开启增量更新并清理旧版本废弃文件（-d）
.\depotdownloader.exe -lu China -d ./813230.zip
```

### 2. 使用纯 HTTP 模式降低 TLS 握手开销
```powershell
.\depotdownloader.exe --use-http -lu China -p 813230.zip -o ./my_game
```

### 3. 正则表达式过滤，仅下载指定文件
```powershell
# 仅下载主程序 exe 和动态链接库
.\depotdownloader.exe -lu China -p 813230.zip -pattern ".*(exe|dll)$"
```

### 4. 单独下载指定清单与密钥
```powershell
.\depotdownloader.exe -l -u China -m "813231_8951799273146338346.manifest" -k "bb42c41063ff494ceca8a0ce69218fcc97cc5f31c2066992f80ec8dab229486e"
```

### 5. 指定自选 CDN 加速节点
```powershell
.\depotdownloader.exe -lu China -s "dl.steam.clngaa.com" -p ./813230.zip
```

---

## 命令行参数一览

| 参数 | 缩写 | 默认值 | 说明 |
| :--- | :---: | :---: | :--- |
| `--login-anonymously` | `-l` | `false` | 启用 Steam 匿名登录，自动向 Steam 网络获取与刷新 CDN Auth Token |
| `--api-host` | `-u` | `Public` | Steam WebAPI 接口域名（`Public` / `China` / `Partner` 或自定义域名） |
| `--app-path` | `-p` | `""` | 包含清单与密钥的解压目录路径，或直接指定 `.zip` 压缩包路径 |
| `--manifest-path` | `-m` | `""` | 单 Depot 模式：指定 `.manifest` 二进制清单文件路径 |
| `--depot-key` | `-k` | `""` | 单 Depot 模式：指定 64 位十六进制 Depot 仓库密钥 |
| `--output` | `-o` | 自动识别 | 下载输出目录（默认自动采用 AppID 或 DepotID 命名） |
| `--thread` | `-t` | `32` | 并发下载协程数 |
| `--retry` | `-r` | `5` | 单个分块下载失败的最大重试次数 |
| `--integrity` | `-i` | `false` | 下载完成后按清单对所有文件进行全量 SHA-1 哈希校验 |
| `--delete` | `-d` | `false` | 增量下载时删除输出目录中存在但清单中已被移除的废弃文件 |
| `--pattern` | 无 | `""` | 正则表达式过滤，仅下载符合匹配规则的文件路径 |
| `--server` | `-s` | 官方调度 | 自定义 CDN 节点列表（多节点逗号分隔） |
| `--use-http` | 无 | `false` | WebAPI 请求与 CDN Chunk 下载采用 HTTP 代替 HTTPS，避开 TLS 开销 |
| `--use-websocket` | 无 | `true` | 使用 WebSocket 协议连接 Steam CM 网络（默认开启） |
| `--cell-id` | `-c` | `0` | Steam Cell ID 地理区域编号（0 表示由服务器自动探测） |
| `--app-id` | `-a` | 自动探测 | Steam App ID（缺省时自动从 lua 脚本或目录名中解析） |
| `--help` | `-h` | `false` | 显示命令行帮助文档 |
