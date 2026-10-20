# ZDownloader

![License MIT](https://img.shields.io/badge/license-MIT-blue)
![Version](https://img.shields.io/badge/version-1.7.3-blue)
![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20macOS%20%7C%20Linux-lightgrey)

[English](README.md) | 简体中文

免费开源的多协议下载工具。一个程序文件,双击就能用——HTTP 多线程下载、BT/磁力、
m3u8 视频、RSS 订阅都装在里面,自带桌面管理界面,还可以用浏览器扩展接管浏览器里的下载。

## 功能

**HTTP 下载**
- 多线程分块下载,最高 64 线程,榨干带宽
- 断点续传,重启电脑后接着下
- 可设全局 / 单任务限速
- 支持 HTTP / SOCKS5 代理,可按用途分开启用
- 自动识别文件名与大小,绝不覆盖已有文件

**视频与流媒体**
- 粘贴 m3u8 链接即可下载,自动选最高画质、自动解密、合并为单个文件

**BT / 磁力**
- 磁力链接、.torrent 种子(v1 / v2)都支持
- 文件列表里勾选想要的文件,只下需要的部分
- 完成后可自动做种,按分享率 / 时长自动停止
- 自动封禁吸血、造假的恶意 peer

**日常使用**
- 浏览器扩展:自动接管浏览器下载,嗅探网页里的视频音频,右键直接下载
- RSS 订阅:关注的资源更新后自动下载
- 系统托盘、悬浮速度窗、开机自启、深色 / 浅色主题
- 下载完成系统通知
- magnet 链接和 .torrent 双击直接唤起,已运行时不重复启动

## 支持平台

| 平台 | 状态 |
|---|---|
| Windows 10/11 | ✅ 完整支持 |
| macOS 11+(Intel / Apple Silicon) | ✅ 完整支持 |
| Linux | ⚠️ 可用,桌面集成(托盘等)未适配 |

## 安装

从 [Releases](../../releases) 下载对应平台的压缩包,解压后直接运行:

- **Windows**:双击 `zdownloader.exe`
- **macOS**:打开 `ZDownloader.app`
- **Linux**:运行 `zdownloader`,浏览器访问界面

不想用桌面窗口时,可以加 `-headless` 参数以纯服务方式运行(适合 NAS / 服务器)。

### 安装浏览器扩展(可选)

Chrome 打开 `chrome://extensions` → 开启「开发者模式」→「加载已解压的扩展程序」→
选择压缩包里的 `extension/` 目录。

### 命令行参数

| 参数 | 说明 |
|---|---|
| `-port N` | 指定端口(默认 8620,被占用自动顺延) |
| `-dir PATH` | 指定下载目录 |
| `-data PATH` | 指定数据目录 |
| `-no-tray` | 不显示托盘图标 |
| `-headless` | 无界面模式,仅提供 HTTP 服务 |

## 从源码构建

需要 Go 1.25+ 和 Node.js 18+(macOS 另需 Xcode Command Line Tools):

```bash
# macOS
./scripts/build.sh

# Windows
powershell -ExecutionPolicy Bypass -File scripts\build.ps1
```

构建产物在 `dist/` 目录,含可执行程序、浏览器扩展和 `.app` 包。

## 数据与隐私

- 所有下载记录和配置只保存在你的电脑上,不上传任何服务器
- 界面服务只监听本机(`127.0.0.1`),局域网内其他设备无法访问
- 浏览器扩展的设置保存在 Chrome 本地

## 路线图

- [ ] DASH (.mpd) 视频支持
- [ ] TS 转封装 MP4
- [ ] 下载队列优先级 / 定时下载
- [ ] Linux 桌面集成
- [ ] 多语言界面

## 许可证

[MIT](LICENSE) © mbzmhs
