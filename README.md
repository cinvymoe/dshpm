# dshpm

`dshpm` 是一个独立的 Go 命令行工具，用来管理安装在一个 DeepSeek Harness（DSH）profile 里的插件：

- **区分本地版本和远程版本**：同时显示本地已安装版本 / 本地 checkout 的 commit，以及 npm registry 或 git 上游的最新版本。
- **备份**：备份 profile 的 `package.json`、`pnpm-lock.yaml`、`cordis.patch.yml` 等装配文件；git 本地插件保存为 patch，非 git 本地插件复制源码文件。
- **同步与升级**：本地插件分为 `local`（默认无远程 Git；`sync` 发现未提交修改就拒绝并报告状态，不 stash/rebase；`upgrade` 也要求 checkout 干净）和 `local-remote`（带远程 Git 的本地 checkout，不管理本地修改；`sync` 始终把远程版本检出到独立 worktree，`upgrade` 构建该 worktree 并切换 profile）。`update` 保留旧的 patch 流程。
- **卸载与历史还原**：卸载会清理 profile 依赖、bundle 与 `cordis.patch.yml` 引用并保留本地源码；`history` / `rollback` 可以按备份还原到任意历史状态。
- **发布本地插件**：`publish` 使用 `gh` 把本地插件 checkout 初始化成 Git 仓库、创建 GitHub 仓库并推送，可选打 tag 和创建 Release；发布成功后默认把 profile 依赖切到 `github:OWNER/REPO` 远程管理。
- **不改 DSH 源码**：`dshpm` 只操作 `~/.dsh/profiles/<profile>` 和本地插件 checkout，不修改 DeepSeek Harness 仓库，也不会 commit 或 push 插件仓库。

> 参考 [clidot](https://github.com/cinvymoe/clidot) 的思路：一个可被 clidot 纳管的独立 CLI 工具，而不是一个 DSH 插件。

## 安装

从源码构建：

```sh
git clone https://github.com/cinvymoe/dshpm.git
cd dshpm
go build -trimpath -ldflags "-s -w -X github.com/cinvymoe/dshpm/internal/version.Version=0.5.8" -o dshpm ./cmd/dshpm
install -m 0755 dshpm ~/.local/bin/dshpm
```

也可以直接用 clidot 纳管（见文末）。

## 快速开始

```sh
# 查看默认 profile（优先 web）中的全部插件
dshpm list

# 标记一个需要手工维护的插件为“不管理”；update/sync/upgrade 会跳过它
dshpm unmanage dsh-context

# 恢复管理
dshpm manage dsh-context

# 把带远程的本地插件切换为 local 模式；auto 恢复自动分类
dshpm mode local dsh-superagent
dshpm mode auto dsh-superagent

# 查看某个 profile，输出机器可读 JSON
dshpm list --profile web --json

# 备份 profile 与本地插件 patch
dshpm backup

# 更新一个 npm 插件前先做计划
dshpm update dsh-context --dry-run

# 更新指定的 npm / git 插件
dshpm update dsh-context @indexyz/dsh-autocompact

# 同步本地 git 插件；local 有脏文件会拒绝，local-remote 用 worktree
dshpm sync --all --dry-run
dshpm sync --all

# 升级插件；本地插件在 worktree 存在时构建并切到 worktree
dshpm upgrade --all

# 旧版 patch 流程仍保留
dshpm update --all

# 卸载插件：移除 profile 依赖、bundle 与 cordis.patch 引用，保留本地源码
dshpm uninstall dsh-context

# 查看历史备份，并把最近一次备份还原成计划
dshpm history
dshpm rollback --apply

# 发布一个本地插件到 GitHub（默认 private；--public 用 --visibility public）
dshpm publish dsh-message-model-badge --dry-run
dshpm publish dsh-message-model-badge

# 查看环境
dshpm doctor
```

## 版本语义

`dshpm list` 的 `LOCAL VERSION` 与 `REMOTE VERSION` 两列：

| 来源 | 本地版本 | 远程版本 |
| --- | --- | --- |
| npm (`1.2.3`, `^1.2.3`, `latest`) | `node_modules/<包名>/package.json` 的 `version` | npm registry 的 `dist-tags.latest` |
| git (`github:owner/repo`) | `pnpm-lock.yaml` 中该依赖解析出的 commit | `git ls-remote <repo> HEAD` 的 commit |
| 本地 `local` (`link:` / `file:`，无远程) | 本地 checkout 的 `package.json` 版本（若是 git 仓库则附 `HEAD`） | `-` |
| 本地 `local-remote` (`link:` / `file:`，带远程 Git) | 本地 checkout 的 `package.json` 版本 + git `HEAD` | 首个配置 remote 的 `HEAD` |
| tarball (`.tgz`) | 已安装版本 | `-`（没有可查询的更新源） |

`KIND` 列区分 `local`、`local-remote`、`git`、`registry`、`tarball`。`CHANGES` 列显示本地 checkout 的未提交文件数、ahead/behind 提交数；使用远程 worktree 时显示 `worktree:<ref>`。`STATUS` 列会给出 `up-to-date`、`update`、`local-only`、`local-remote`、`dirty`、`ahead`、`diverged`、`worktree`、`conflict`、`pinned` 等状态。

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

## 卸载插件

```sh
# 只显示将要卸载什么，不修改文件
dshpm uninstall dsh-context --dry-run

# 卸载一个或多个插件；默认先备份
dshpm uninstall dsh-context @indexyz/dsh-autocompact

# 跳过卸载前备份（不推荐）
dshpm uninstall dsh-context --no-backup
```

卸载会调用 `pnpm remove`，清理 profile 的 `package.json` 中 `dsh.profile.bundles` 条目、`cordis.patch.yml` 中引用该插件的顶层条目（包括 `- insert:` 列表与 `- id: ...` 覆盖），并移除 `node_modules` 中残留的本地链接。**本地源码目录不会被删除**，之后可以重新安装或手动删除。卸载完成的插件需要重启 DSH 才会从运行时消失。

## 历史与回滚

`dshpm history` 默认只列当前 profile 的备份；`--all` 查看所有 profile，`--limit N` 控制条数，`--json` 输出机器可读结果。

```sh
# 查看历史
dshpm history

# 默认选择最新备份，先输出计划
dshpm rollback

# 还原指定的历史备份（完整 ID 或唯一前缀）
dshpm rollback 20261103-120000-web --apply

# 还原后不执行 pnpm install；或覆盖 checkout 的未提交改动
dshpm rollback <id> --apply --no-install
dshpm rollback <id> --apply --force
```

回滚会先为当前状态创建一个 checkpoint 备份（`--no-backup` 可关闭），再恢复目标备份的 profile 文件、本地 patch 与非 git 插件源码。除非指定 `--no-install`，随后会运行 `pnpm install --frozen-lockfile` 物化依赖。若 patch 贴回发生冲突，命令以退出码 `1` 结束并列出冲突文件，不再继续安装。

## 发布本地插件

`publish` 只接受 `link:` / `file:` / `workspace:` 的本地插件，并且要求已登录 `gh auth login`。它会：

1. 在插件目录执行 `git init`（如果还不是 Git 仓库）；
2. 缺少 `.gitignore` 时写入 `node_modules/`、`dist/` 等忽略规则（保留 `lib/` 构建产物，便于仓库直接安装）；
3. 暂存并提交当前工作树（默认提交信息 `chore: publish via dshpm`）；
4. 用 `gh repo create` 创建仓库（默认 private），添加 remote 并 push 当前分支；
5. 可选 `--tag` 推送 `v<package version>`，`--release` 同时创建 GitHub Release；
6. 默认把当前 profile 的依赖从 `link:` 改写为 `github:OWNER/REPO` 并执行 `pnpm install`，之后由远程仓库管理；失败时自动恢复原依赖。

```sh
# 先看计划和目标仓库，不修改任何文件
dshpm publish dsh-message-model-badge --dry-run

# 发布为私有仓库
dshpm publish dsh-message-model-badge

# 发布为公开仓库，并创建 v0.1.0 Release
dshpm publish dsh-message-model-badge --visibility public --release

# 发布全部本地插件（每个插件一个仓库）
dshpm publish --all-local --visibility private
```

常用参数：

| 参数 | 说明 |
| --- | --- |
| `--visibility private\|public\|internal` | 仓库可见性，默认 `private` |
| `--repo OWNER/NAME` | 指定仓库名，只能用于单个插件 |
| `--owner OWNER` | 指定 GitHub 账号/组织；默认取 `gh api user` |
| `--branch NAME` | 推送分支，默认 `main` |
| `--remote NAME` | 指定 remote；默认优先 `origin`，已存在其他 remote 时用 `dshpm` |
| `--message TEXT` | 覆盖提交信息 |
| `--tag` / `--release` | 打 tag；`--release` 还会创建 GitHub Release |
| `--force` | 允许大文件或替换同名 remote |
| `--no-backup` | 跳过发布前备份 |
| `--no-switch` | 发布后保留本地 `link:` 依赖，不切换到远程仓库 |

发布成功后默认切换为远程管理：profile 依赖会变成 `github:OWNER/REPO`，并执行一次 `pnpm install` 让本地 `node_modules` 改为远程 checkout。如果你还要继续本地开发，使用 `--no-switch`；`--dry-run` 只展示计划，不会改写依赖。

## 同步与升级

本地插件按是否有远程 Git 分为两类：

- `local`：默认是纯本地目录（`link:` / `file:`，没有 `origin` remote）。升级直接在本地目录构建，不进行 rebase，也不创建 worktree；如果本地目录是 git checkout，任何未提交修改都会阻止 `sync` 和 `upgrade` 并报告状态。通过 `dshpm mode local` 强制覆盖带 remote 的插件时同样适用，`sync` 不创建 worktree。
- `local-remote`：带远程 Git（`origin` 或其他 remote）的本地 checkout。dshpm **不管理**本地修改，不 rebase、不 patch、不删除本地提交；`sync` 始终把远程版本检出到独立 worktree（默认 `/data/project/dshpm-worktrees/<profile>/<plugin>`），`upgrade` 只构建该 worktree，成功后把 profile 依赖切到 `link:<worktree>` 并执行 `pnpm install`。本地 checkout 完全不动。

推荐顺序：先 `dshpm sync`，再 `dshpm upgrade`。

`sync` 处理本地 checkout：

- `local`：`fetch --all --prune` 后解析 upstream；如果 `git status --porcelain` 有任意输出，直接返回 `blocked` 并报告脏文件数量，不做 stash、不 rebase、不改变工作区；干净时才执行 fast-forward 或 rebase，rebase 冲突会 abort 并恢复到执行前 HEAD，不留下冲突现场。
- `local-remote`：`fetch --all --prune` 后解析 upstream；已注册的 worktree 已指向目标 commit 时显示 `up-to-date`，否则检出目标到 worktree；本地 checkout 不参与 fast-forward/rebase，也不会被修改。
- 没有 remote 的 `local` 显示 `local-only`，直接跳过。

`upgrade` 复用 `update` 的 npm/git 升级路径。本地插件的构建规则：

- `local`：直接构建本地目录；git checkout 有未提交修改时返回 `blocked` 并报告状态，`--build-source local` 是唯一有效的方式（`auto` 也走本地），非 git 本地目录不要求 git 仓库。
- `local-remote`：始终使用 `sync` 生成的 worktree；没有 worktree 时返回 `worktree-missing` 并提示先执行 `dshpm sync <name>`，不会偷偷构建本地目录；`--build-source local` 对 `local-remote` 不生效。
- `--build-source worktree` 只对 `local-remote` 有意义；`--build-source auto`（默认）对 `local` 走本地、对 `local-remote` 走 worktree。

`--no-build` 只更新依赖，不运行构建；存在 worktree 时也不会切换 profile。

```sh
# 先看同步计划（local 有脏文件会 blocked，local-remote 会建 worktree）
dshpm sync --all --dry-run

# 同步全部本地 git 插件
dshpm sync --all

# 升级全部插件；local 直接构建，local-remote 需要先 sync
dshpm upgrade --all

# 只构建 local-remote 的 worktree（适合本地有改动）
dshpm upgrade dsh-superagent --build-source worktree

# 纯 local 插件直接构建本地目录
dshpm upgrade dsh-codex-mobile --build-source local

# 只升级指定插件，跳过构建
dshpm upgrade dsh-context @dsh-external/dsh-recent-chats --no-build
```

## 管理与不管理

dshpm 默认管理 profile 中的全部依赖。需要手工维护或暂时不要自动升级的插件，可以标记为“不管理”：

```sh
# 标记为不管理（仍会出现在 list 中，但 update/sync/upgrade 会跳过）
dshpm unmanage @dsh-external/dsh-plugin-manager

# 恢复管理
dshpm manage @dsh-external/dsh-plugin-manager

# 查看 MANAGED 列：yes 表示会被管理，no 表示跳过自动操作（no 排在最后）
dshpm list
```

`unmanaged` 列表保存在 `~/.config/dshpm/config.json` 的 `unmanaged` 数组中；未列出的插件默认都是 managed。`dshpm unmanage` / `dshpm manage` 会校验插件名必须属于当前 profile，并自动创建配置文件目录。

## 本地模式切换

带远程 Git 的本地 checkout 默认自动识别为 `local-remote`。如果希望把它按 `local` 处理（直接在本地目录构建、不创建 worktree），可以显式切换：

```sh
dshpm mode local dsh-superagent
dshpm mode auto dsh-superagent
```

`local` 覆盖保存在 `~/.config/dshpm/config.json` 的 `modes` 对象中，例如 `{"dsh-superagent":"local"}`；`auto` 会删除覆盖并恢复自动分类。切换到 `local` 后，`upgrade` 直接在本地目录构建；`sync` 对带 remote 的 checkout 不创建 worktree，有未提交修改时直接 `blocked`，干净时才 rebase。

## 更新安全规则

1. 修改任何文件前，先自动创建一次备份（`--no-backup` 可关闭）。
2. npm / git 依赖使用 `pnpm add`，只更新当前 profile 的依赖与 lockfile。
3. 本地插件：
   - `local` 的 `upgrade` 直接在本地目录构建，不执行 rebase；`sync` 对带 remote 的 local 先检查工作区，有脏文件直接 `blocked`，不 stash、不 rebase，干净时才同步；
   - `local-remote` 由 `sync` 把远程版本检出到独立 worktree，`upgrade` 只构建 worktree 并把 profile 依赖切到 `link:<worktree>`，本地 checkout 完全不动；
   - 旧的 `update` 仍对 pure local git checkout 使用 patch 流程：未提交改动用临时 index 采集为 patch，不碰暂存区；只允许 fast-forward（没有本地提交）时更新；更新前创建 `refs/dshpm/backup/<时间>` 备份 ref；快进后重新贴回 patch；冲突时停在上游并报告冲突文件，不改写远端。
4. 本地提交（ahead）不会被自动丢弃；需要手动处理后再更新。
5. 更新流程不执行 `git commit`、`git push`，也不删除远端分支；只有显式 `dshpm publish` 会提交并推送本地插件仓库。

## 配置

可选配置文件 `~/.config/dshpm/config.json`：

```json
{
  "dshHome": "~/.dsh",
  "defaultProfile": "web",
  "backupDir": "~/.config/dshpm/backups",
  "worktreeRoot": "/data/project/dshpm-worktrees",
  "unmanaged": [],
  "modes": {},
  "npmRegistry": "https://registry.npmjs.org",
  "pnpmRegistry": "https://registry.npmjs.org",
  "pnpm": "pnpm",
  "git": "git",
  "gh": "gh"
}
```

`unmanaged` 是永不自动更新/同步/升级的插件名列表；`modes` 是本地插件的模式覆盖（目前支持 `{"dsh-superagent":"local"}`）；`npmRegistry` 用于查询版本；`pnpmRegistry` 会作为 `pnpm add --registry` 传入，适合本机 npm 镜像不可用时切回官方源。`worktreeRoot` 是 `local-remote` 同步时存放远程 worktree 的根目录。`dshpm init` 会写入一份默认配置。所有命令也支持 `--dsh-home`、`--profile` 和环境变量 `DSHPM_PROFILE`。

## 命令一览

| 命令 | 作用 |
| --- | --- |
| `dshpm list` / `status` | 插件清单、本地/远程版本、管理状态、变更状态 |
| `dshpm manage <name...>` | 标记插件为 managed |
| `dshpm unmanage <name...>` | 标记插件为 unmanaged（跳过自动升级/同步/更新） |
| `dshpm mode local\|auto <name...>` | 切换本地插件的模式（local 覆盖 / 恢复 auto） |
| `dshpm backup` | 备份 profile 文件与本地插件 patch |
| `dshpm restore <dir>` | 恢复备份（默认只做计划） |
| `dshpm uninstall <name...>` / `remove` | 卸载插件并清理 profile 引用，保留本地源码 |
| `dshpm history` | 列出历史备份 |
| `dshpm rollback [id]` | 还原历史备份（默认只做计划） |
| `dshpm publish <name...>` / `--all-local` | 通过 gh 发布本地插件；`--dry-run` 只做计划 |
| `dshpm sync <name...>` / `--all` | 同步本地 checkout：local 有脏文件时 blocked，干净时 rebase；local-remote 到远程 worktree |
| `dshpm upgrade <name...>` / `--all` | 升级插件；`local` 直接构建，`local-remote` 构建 worktree |
| `dshpm update <name...>` / `--all` | 旧的 patch 式更新；`--dry-run` 只做计划 |
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
