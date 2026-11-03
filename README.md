# dshpm

`dshpm` 是一个独立的 Go 命令行工具，用来管理安装在一个 DeepSeek Harness（DSH）profile 里的插件：

- **区分本地版本和远程版本**：同时显示本地已安装版本 / 本地 checkout 的 commit，以及 npm registry 或 git 上游的最新版本。
- **备份**：备份 profile 的 `package.json`、`pnpm-lock.yaml`、`cordis.patch.yml` 等装配文件；git 本地插件保存为 patch，非 git 本地插件复制源码文件。
- **同步更新**：npm 插件通过 pnpm 升级；git 依赖重新解析上游；本地 checkout 在保留未提交改动的前提下快进到上游。
- **不改 DSH 源码**：`dshpm` 只操作 `~/.dsh/profiles/<profile>` 和本地插件 checkout，不修改 DeepSeek Harness 仓库，也不会 commit 或 push 插件仓库。

> 参考 [clidot](https://github.com/cinvymoe/clidot) 的思路：一个可被 clidot 纳管的独立 CLI 工具，而不是一个 DSH 插件。

## 安装

从源码构建：

```sh
git clone https://github.com/cinvymoe/dshpm.git
cd dshpm
go build -trimpath -ldflags "-s -w -X github.com/cinvymoe/dshpm/internal/version.Version=0.1.0" -o dshpm ./cmd/dshpm
install -m 0755 dshpm ~/.local/bin/dshpm
```

也可以直接用 clidot 纳管（见文末）。

## 快速开始

```sh
# 查看默认 profile（优先 web）中的全部插件
dshpm list

# 查看某个 profile，输出机器可读 JSON
dshpm list --profile web --json

# 备份 profile 与本地插件 patch
dshpm backup

# 更新一个 npm 插件前先做计划
dshpm update dsh-context --dry-run

# 更新指定的 npm / git 插件
dshpm update dsh-context @indexyz/dsh-autocompact

# 更新所有有更新的插件（会先自动备份）
dshpm update --all

# 查看环境
dshpm doctor
```

## 版本语义

`dshpm list` 的 `LOCAL VERSION` 与 `REMOTE VERSION` 两列：

| 来源 | 本地版本 | 远程版本 |
| --- | --- | --- |
| npm (`1.2.3`, `^1.2.3`, `latest`) | `node_modules/<包名>/package.json` 的 `version` | npm registry 的 `dist-tags.latest` |
| git (`github:owner/repo`) | `pnpm-lock.yaml` 中该依赖解析出的 commit | `git ls-remote <repo> HEAD` 的 commit |
| 本地目录 (`link:` / `file:`) | 本地 checkout 的 `package.json` 版本 + `git HEAD` | 本地 checkout `origin` 的 `HEAD` |
| tarball (`.tgz`) | 已安装版本 | `-`（没有可查询的更新源） |

`CHANGES` 列显示本地 checkout 的未提交文件数、ahead/behind 提交数。`STATUS` 列会给出 `up-to-date`、`update`、`local-only`、`dirty`、`ahead`、`diverged`、`pinned` 等状态。

## 备份格式

每次备份是一个目录：

```text
~/.config/dshpm/backups/20261103-120000-web/
├── backup.json              # 备份清单（profile、时间、工具版本、插件列表）
├── profile/
│   ├── package.json
│   ├── pnpm-lock.yaml
│   ├── pnpm-workspace.yaml
│   ├── cordis.patch.yml
│   └── cordis.yml
└── plugins/
    ├── _dsh-external_dsh-upgrade/
    │   └── changes.patch    # git diff --binary HEAD（含未跟踪文件）
    └── plain-plugin/
        └── files/           # 非 git 本地插件的源码副本（不含 node_modules/.git）
```

恢复：

```sh
# 只显示将要恢复什么
dshpm restore ~/.config/dshpm/backups/20261103-120000-web

# 实际恢复；已有 profile 文件会先保存到 .dshpm-restore-<时间>/
dshpm restore ~/.config/dshpm/backups/20261103-120000-web --apply

# 覆盖 checkout 现有未提交改动，或覆盖已存在的非 git 插件目录
dshpm restore <backup> --apply --force
```

## 更新安全规则

1. 修改任何文件前，先自动创建一次备份（`--no-backup` 可关闭）。
2. npm / git 依赖使用 `pnpm add`，只更新当前 profile 的依赖与 lockfile。
3. 本地 checkout：
   - 先 `git fetch --all --prune`；
   - 未提交改动用临时 index 采集为 patch，不碰暂存区；
   - 只允许 fast-forward（没有本地提交）时更新；
   - 更新前创建 `refs/dshpm/backup/<时间>` 备份 ref；
   - 快进后重新贴回 patch；冲突时停在上游并报告冲突文件，不改写远端；
   - 若配置了 `build` 脚本则执行构建，构建失败自动回滚到更新前状态。
4. 本地提交（ahead）不会被自动丢弃；需要手动处理后再更新。
5. 不执行 `git commit`、`git push`，也不删除远端分支。

## 配置

可选配置文件 `~/.config/dshpm/config.json`：

```json
{
  "dshHome": "~/.dsh",
  "defaultProfile": "web",
  "backupDir": "~/.config/dshpm/backups",
  "npmRegistry": "https://registry.npmjs.org",
  "pnpmRegistry": "https://registry.npmjs.org",
  "pnpm": "pnpm",
  "git": "git"
}
```

`npmRegistry` 用于查询版本；`pnpmRegistry` 会作为 `pnpm add --registry` 传入，适合本机 npm 镜像不可用时切回官方源。`dshpm init` 会写入一份默认配置。所有命令也支持 `--dsh-home`、`--profile` 和环境变量 `DSHPM_PROFILE`。

## 命令一览

| 命令 | 作用 |
| --- | --- |
| `dshpm list` / `status` | 插件清单、本地/远程版本、变更状态 |
| `dshpm backup` | 备份 profile 文件与本地插件 patch |
| `dshpm restore <dir>` | 恢复备份（默认只做计划） |
| `dshpm update <name...>` / `--all` | 更新插件；`--dry-run` 只做计划 |
| `dshpm profiles` | 列出 profile 与依赖数量 |
| `dshpm doctor` | 检查 DSH 目录、git、pnpm |
| `dshpm init` | 写默认配置文件 |
| `dshpm version` | 打印版本 |

## 退出码

- `0`：成功
- `1`：执行失败（网络、备份、更新、构建或冲突）
- `2`：命令行参数错误

## 用 clidot 纳管

仓库推送后可以用 clidot 把它作为一个纯 CLI 工具纳管（无 systemd 单元）：

```sh
clidot deploy --repo https://github.com/cinvymoe/dshpm.git
```

`dshpm` 自身不依赖 DSH 进程，因此可以在 DSH 运行或停止时使用。更新插件后重启 DSH 才会加载新插件，例如 `clidot service dsh restart`。

## 开发

```sh
go test ./...
go vet ./...
go build ./cmd/dshpm
```
