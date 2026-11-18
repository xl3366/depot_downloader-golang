# Depot Downloader (Go 版)

这是一个使用 Go 语言编写的高性能 Steam Depot 下载器。基于原版 Python 脚本和 Rust 实现重构，旨在提供极速、稳定、跨平台的游戏文件与 Steam 清单下载体验。

## ✨ 核心特性

- **⚡ 极速并发架构**：采用“下载池 + 解码写盘池”的双级流水线架构，极大提升吞吐量（实测可达数十 MB/s）。
- **🔄 真正的增量更新（断点续传）**：永久保留 `.depot_xxx.resume` 隐藏缓存文件，结合 Steam 的 Chunk 机制，游戏更新时**只下载变动的块**，旧文件没变的部分直接复用。
- **🛡️ 企业级容错机制**：
  - 2 秒定时异步持久化下载进度，强杀进程/断电不丢进度。
  - 单次请求 30 秒超时 + 指数退避重试 + CDN 节点自动切换（防止 403 或节点抽风）。
- **📦 一键解析 SteamCMD 目录**：支持直接传入包含 `.lua`、`.acf` 和多个 `.manifest` 的文件夹，自动提取 AppID 和所有 Depot Key，无视缺 Key 或缺文件的报错，能下尽下。
- **💻 跨平台路径处理**：自动适配 Windows/Linux/macOS 的路径分隔符，防止文件挤压，完美还原游戏目录结构。
- **🕵️ 智能解密与解压**：内置 AES-256-CBC 解密，原生支持 Steam 的 VZa (LZMA)、VSZa (ZSTD) 和 Zip 三种 Chunk 压缩格式。

## 🚀 快速开始

### 编译
确保你的系统已安装 Go 1.22+：
```bash
go mod tidy
go build -o depotdl .
```

使用方式

1. 单文件模式（下载单个 Manifest）

```bash
./depotdl -m 5335511_43932991698.manifest -k b18bb1bf8efc42b436bf04149ef7b8493bdffdb8e478370ee192665a27146d4f -o ./output
```

2. 目录模式（自动解析 SteamCMD 清单目录）
如果你有一个包含了 5335510.lua 和多个 .manifest 文件的目录，可以直接指向该目录：

```bash
./depotdl -m ./manifests_dir -o ./game_download
```

程序会自动读取 lua 中的 Key，并自动跳过没权限或缺失的 Depot，下载所有可用的文件。

3. 极速 CDN 指定（推荐中国大陆及亚洲用户）

```bash
./depotdl -m ./manifests_dir -o ./game -u steampipe-kr.akamaized.net
```

(也可以使用 -u dl.steam.clngaa.com 或 -u st.dl.eccdnx.com)

参数说明

参数 全称 默认值 说明
-m --input 必填 Manifest 文件路径 或 包含 lua/acf 的目录路径
-k --key 空 Depot 解密密钥 (如果使用目录模式且目录内有 lua，可不填)
-o --output default 输出目录 (默认为 ./<AppID> )
-u --url 自动获取 指定 CDN 节点，多个用逗号隔开
-p --proxy 空 代理地址 (如 socks5://127.0.0.1:1080)
-f --filter 空 正则/文件名过滤，只下载特定文件 (如 -f "*.dll,*.exe")
-r --retry 3 单个 Chunk 下载失败的最大重试次数

📂 断点缓存说明

下载过程中会在当前目录生成一个 .depot_<ID>.resume 文件，它记录了已成功写入的 Chunk 的 SHA 值。

· 请勿随意删除：下次更新游戏时，程序靠它实现“只下新块”的增量更新。
· 何时删除：如果你完全不需要增量更新，或者要彻底重新下载整个游戏，可以手动删掉该文件。

📜 致谢

· 思路源自 ValvePython/steam、SteamKit2 以及 detiam/steam_websocket。
· 以上内容均由AI生成