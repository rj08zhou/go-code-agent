# MCP 配置示例

本目录提供一个最小可运行的 MCP 配置示例：`.mcp.json.example`。

## 它是什么

`go-code-agent` 会在**启动工作目录（workdir）根**自动读取 `.mcp.json`，
把其中声明的每个 server 作为子进程拉起，并通过 `initialize` + `tools/list`
自动发现工具，注册为 `mcp__<server>__<tool>` 供 LLM 调用。

> 注意：文件名必须是 **`.mcp.json`**（带前导点），并且放在你**运行 `./agent`
> 的那个目录**下才会被加载。本示例用 `.mcp.json.example`（带 `.example`
> 后缀）是为了**防止被自动加载**，你复制后把 `.example` 去掉即可启用。

## 怎么用

1. 复制示例并去掉后缀，放到你的工作目录根：

   ```bash
   cp examples/mcp/.mcp.json.example /path/to/your/workdir/.mcp.json
   ```

2. 把示例里的 `/ABSOLUTE/PATH/TO/A/DIRECTORY` 改成你**真实、绝对路径**
   的目录（filesystem server 只会暴露这个目录下的文件，且必须是绝对路径）。

   ```json
   {
     "servers": {
       "fs": {
         "command": "npx",
         "args": ["--yes", "@modelcontextprotocol/server-filesystem", "/Users/you/projects"],
         "env": {}
       }
     }
   }
   ```

3. 在该工作目录运行 agent：

   ```bash
   cd /path/to/your/workdir
   /path/to/agent
   ```

4. 启动后工具 `mcp__fs__read_file`、`mcp__fs__list_directory` 等即可被使用。
   也可在 REPL 中查看/管理：

   ```
   /mcp                          # 列出已连接的 server 与工具
   /mcp connect fs npx --yes @modelcontextprotocol/server-filesystem /tmp   # 运行时连
   /mcp disconnect fs            # 运行时断开
   ```

## 前置要求

- 已安装 Node.js（示例用到 `npx` 拉取官方
  `@modelcontextprotocol/server-filesystem`）。
- 首次运行 `npx` 会下载该 server，可能耗时 10–30 秒，属正常。

## 配置格式

```json
{
  "servers": {
    "<server-name>": {
      "command": "<可执行文件，如 node / python / npx>",
      "args": ["<启动参数...>"],
      "env": { "KEY": "VALUE" }
    }
  }
}
```

- `command`：子进程命令，从 `PATH` 解析。
- `args`：传给 server 的参数。
- `env`：附加环境变量（会合并进进程环境）。

## 更多参考

- 协议与 server 生态：https://modelcontextprotocol.io
- 官方参考 server（filesystem / fetch / ...）：
  https://github.com/modelcontextprotocol/servers
- 项目 README 的「MCP Integration」章节。
