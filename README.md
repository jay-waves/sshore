# sshore

[![CI](https://github.com/jay-waves/sshore/actions/workflows/ci.yml/badge.svg)](https://github.com/jay-waves/sshore/actions/workflows/ci.yml)

一个小而美的 SSH 命令行工具：断开连接，远端 shell 继续运行；重新连接，回到同一个进程。支持多个命名会话，每个会话最多一个客户端连接。

Go 实现，复用系统 OpenSSH。服务端支持 Linux，客户端支持 Linux 和 Windows。使用 smux 协议 v2 在一条 SSH 连接内承载独立的控制流、终端流和文件流；文件传输使用自己的协议，不引入 SFTP，也不依赖或兼容 trzsz。传文件不会另外启动 SSH 或再次询问 SSH 密码。

## 构建与安装

运行预编译程序不需要 Go。每次 CI 成功后，Windows/Linux 的 amd64、arm64 安装包可在 [Actions](https://github.com/jay-waves/sshore/actions) 的 Artifacts 中下载。推送 `v*` 标签会在测试通过后自动生成 [Release](https://github.com/jay-waves/sshore/releases)，附带归档与 SHA-256 校验清单。

从源码构建需要 Go 1.25+，本地还需要 OpenSSH 客户端（`ssh` / `ssh.exe`），远端需要正常运行的 SSH 服务。

```sh
git clone https://github.com/jay-waves/sshore.git
cd sshore
go build -o dist/sshore .

# Linux 上交叉编译 Windows 客户端
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -buildvcs=false -o dist/sshore.exe .
```

把 Linux 二进制安装到远端 PATH，例如 `~/.local/bin/sshore`；本地安装对应平台的二进制。建议两端使用相同版本；连接时会检查协议版本。不需要 root，也不另外监听网络端口。

0.2 使用新的复用协议，与 0.1 的连接协议不兼容。如果之前有正在运行的 0.1 会话，请先用旧客户端连接并退出这些 shell，再替换两端程序；不能直接给已运行的旧会话进程升级协议。

```sh
# 在 Linux 服务器上安装
install -Dm755 dist/sshore "$HOME/.local/bin/sshore"
```

非交互式 SSH 的 PATH 不一定包含 `~/.local/bin`，可显式指定远端绝对路径：

```sh
sshore myserver --server /home/alice/.local/bin/sshore
```

Windows 客户端应运行在支持 VT 的现代控制台中，例如 Windows Terminal。Windows 二进制是客户端；会话与文件命令在连接后的 Linux shell 内执行。

## 使用

```sh
sshore user@host                 # 创建或恢复 main 会话
sshore myserver -s build         # 创建或恢复 build 会话
sshore myserver list             # 列出该用户的会话、shell PID、连接状态
sshore myserver -J jump -p 2222   # SSH 连接参数
sshore myserver --download-dir ./downloads  # 下载自动保存到指定本地目录
```

读取系统 OpenSSH 的配置、主机密钥和认证设置。可传入 `-p`、`-i`、`-J`、`-F`、`-l`、`-o`、`-v`、`-4`、`-6`，复杂配置推荐放在 `~/.ssh/config`。不是 OpenSSH 全部参数的透传替代品。主机名与内置命令重名时，应为主机配置另一个 SSH 别名。

会话名允许 1–48 个 ASCII 字母、数字、下划线、连字符，首字符必须为字母或数字。已被连接的会话会拒绝第二个客户端，不抢占、不共享输入。

在远端 shell 中：

```sh
sshore detach                   # 断开连接，保留会话
sshore upload                   # 从本地选择一个文件，上传到远端当前目录
sshore upload /tmp/uploads       # 上传到指定远端目录
sshore download ./report.zip     # 下载到本地选择的位置
sshore download a.zip b.log c.png # 批量下载，最多两个并发任务
exit                            # 结束 shell 和该会话
```

常用参数：

| 参数 | 用途 |
| --- | --- |
| `-s, --session NAME` | 指定持久会话，默认 `main` |
| `--server PATH` | 远端 sshore 的路径，默认从 PATH 查找 |
| `--download-dir DIRECTORY` | 已存在的本地目录；自动接收下载，不再逐个询问路径，仍不覆盖文件 |
| `-q, --quiet` | 关闭周期性传输进度，保留提示与完成/错误信息 |
| `--no-escape` | 将 Ctrl-] 发送给远端应用，改用 `sshore detach` 分离 |
| `-h, --help` | 查看帮助，可放在连接参数前后 |
| `-V, --version` | 查看版本 |

`sshore help download` 或 `sshore download --help` 提供子命令帮助。在路径前加 `--` 可以使用以连字符开头的文件名，例如 `sshore download -- -report.txt`。参数错误返回 `2`；其他操作失败返回非零，远端 shell 正常退出时保留其退出码。

本地快捷键：

- `Ctrl-]`：即使远端前台程序正在运行，也可以 detach。
- 文件选择提示期间按 `Ctrl-C`：仅取消当前提示对应的任务。
- 没有路径提示、但仍有文件任务时按 `Ctrl-C`：仅取消最近加入的活动任务，其他任务继续。任务编号会显示在提示和进度中。
- 没有文件任务时，`Ctrl-C` 正常发送给远端程序。detach 或断线则取消该连接的所有文件任务。

文件选择采用纯命令行路径输入，支持包含空格、中文的 UTF-8 路径，也支持外层引号和 `~/`。不会执行命令替换或展开通配符。下载时回车接受当前本地目录下的默认文件名，也可以输入目录或完整目标路径。

同一会话可以同时上传和下载，最多两个任务（包括等待本地路径确认的任务）。例如在远端发起两个下载：

```sh
sshore download ./a.zip &
sshore download ./b.zip &
wait
```

本地依次确认两个保存位置，已确认的任务可以继续传输。手动发起第三个同时活动的任务会收到明确的并发限制错误；`sshore download FILE...` 则使用两个 worker 逐步处理这一批文件，失败的文件不会取消其余文件，最后汇总退出状态。批量任务占用两路容量，不额外创建 SSH 连接。

## 行为与边界

- 保留的是 shell、工作目录、环境变量及仍在运行的程序；不会保存或恢复终端屏幕、滚动历史。重新连接时必要的话按 `Ctrl-L` 重绘。
- 断开期间继续读取 PTY，丢弃输出，避免程序因无人读取输出而阻塞。不会自动重连；再次运行同一个连接命令即可。
- 每个命名会话是一个独立后台进程和一个 PTY，无全局 daemon；shell 退出后会话结束。机器重启、进程被杀或系统登录会话清理策略仍可能结束会话。
- 每个文件任务占用独立 smux 流和 goroutine，可双向并发，单独关闭文件流不会关闭终端或其他文件流。暂不支持目录、符号链接、断点续传、拖放、嵌套 SSH 或协议互操作。
- 文件按 64 KiB 分块，校验大小和 SHA-256，接收端临时文件完成后原子发布，已有文件不覆盖。普通取消和断线会清理临时文件；接收进程被强制杀死或系统崩溃可能留下 `.sshore-*.part`。
- 发布文件需要目标文件系统支持硬链接（例如 Linux 常见文件系统、Windows NTFS）。不支持时明确报错，不回退到可能覆盖文件的操作。文件权限、时间戳等元数据不复制；Linux 接收文件默认权限为 `0600`，Windows 权限继承目标目录的 ACL。
- 这些是应用层逻辑流，不是 OpenSSH 原生 channel。smux v2 提供逐流流控：每流接收窗口为 256 KiB，总接收预算为 2 MiB，smux 帧上限为 16 KiB。文件 I/O 不在终端事件循环中执行，文件字节不经过 PTY。没有硬实时优先级保证；同一 TCP 连接仍可能出现网络队头阻塞。
- 文件提示期间不向远端转发键盘输入，最多缓冲 256 KiB 远端输出；超出会省略并提示。路径提示采用基础行编辑，未提供完整 shell 补全。
- SSH agent/X11 等转发环境来自创建会话的连接，重连时不刷新；首版不保证转发在重连后继续可用。
- OpenSSH 登录脚本若向非交互连接的 stdout 打印内容，会干扰协议握手。请将这类输出限定在交互 shell 中。

## 运行目录

优先使用 `$XDG_RUNTIME_DIR/sshore`，否则使用 `/tmp/sshore-UID`，目录必须属于当前用户且权限为 `0700`。可以通过 `SSHORE_RUNTIME_DIR` 指定已有父目录下的短绝对路径；不要使用很长的路径（Unix socket 路径长度有限）。

每个会话的 `.sock` 是通信入口；`.lock`、`.start` 用于进程生存期及启动互斥；`.log` 记录后台错误。锁文件和日志可以在会话退出后保留，不代表会话仍在运行。会话列表只查询活动 socket。

内部入口 `sshore bridge NAME`、`sshore daemon NAME`、`sshore sessions` 用于 SSH 桥接，不需要日常手动启动。

## 验证

```sh
go test -race ./...
go vet ./...
go build -buildvcs=false -o dist/sshore .
python3 tests/ssh_e2e.py
```

Go 测试覆盖分帧、文件校验、防覆盖、取消清理，以及 Linux 实际后台进程的命名隔离、单连接限制、窗口尺寸、退出码、detach/断线后的 PID 与变量保留。额外覆盖双向并发、第三任务拒绝、故意不读取下载流时上传及终端仍可用、单任务取消隔离和断线清理两个上传。客户端文件 worker 也有独立并发测试。集成测试构建带竞态检测的二进制。

`tests/ssh_e2e.py` 需要 Linux、Python 3、OpenSSH 的 `ssh`、`sshd`、`ssh-keygen`。测试建立临时密钥、配置和仅监听 `127.0.0.1` 的 SSH 服务，验证真实 CLI、PTY、双向文件传输、批量下载、下载目录、防覆盖、路径提示、终端恢复，以及 SSH 密钥口令认证后传文件不重复认证，不修改用户的 SSH 配置或系统服务。

CI 在 Windows 与 Linux 上原生运行 Go 测试和竞态检测；Linux 额外运行真实 SSH/PTY 端到端测试。Windows 输入直接调用 `ReadConsole`，测试覆盖 Ctrl-Z 等控制键和 UTF-16 分段转换；真实 Windows Terminal 下的完整交互仍需要实机试用，不以交叉编译代替这一验证。

## 代码结构

| 文件 | 内容 |
| --- | --- |
| `main.go` | 命令和参数 |
| `client.go`、`terminal_*.go` | SSH 子进程、终端和本地交互 |
| `server_linux.go` | 会话进程、PTY、Unix socket 和 SSH 桥接 |
| `helper_linux.go` | 远端 upload/download/detach 命令 |
| `mux.go` | smux v2 配置、版本握手和 SSH 管道适配 |
| `protocol.go` | 各逻辑流内的消息格式 |
| `client_files.go` | 独立文件任务、取消和进度 |
| `files.go` | 分块收发、哈希校验和原子发布 |

## 开发与发布

保持单一职责：OpenSSH 处理认证与加密，Linux daemon 持有 PTY，smux 处理逻辑流复用，文件协议只负责普通文件的收发和完整性。没有配置文件框架、数据库或额外网络服务。

CI 包括格式检查、`go vet`、两平台竞态测试、Linux SSH 集成测试，以及四个平台目标的纯 Go 构建。安装包包含程序、README 和 MIT 许可证。发布时推送形如 `v0.2.1` 的标签，workflow 自动填入二进制版本；仅所有测试及构建通过后才发布归档和 `SHA256SUMS`。

MIT License。
