# Design: 删除 legacy runtime 实现（pi 为唯一方向）

Date: 2026-09-03

## 目标

repo 收敛到单一 runtime 方向：**pi**（尚待实现）。删除 Go 参考 runtime
（cmd/agent-runtime）与 Node runtime（cmd/node-agent-runtime）及其全部
引用。coding-agent（opencode）镜像保留，直到 pi runtime 能替代它。

## 在途工作保全（删除前执行）

- 建分支 `archive/node-agent-runtime`（自当前 HEAD），提交：
  - cmd/node-agent-runtime 的全部未提交改动（含 web/ 新目录、web.html
    删除、测试改动）
  - config/samples/travel/demo/ox-alpha-model.yaml（untracked，属该演示
    的在途工作）
- cmd/registry/main.go 的 `/v1/models` WIP 不属于本次删除，留在工作区
  不动。

## 删除清单

- `cmd/agent-runtime/`、`cmd/node-agent-runtime/`
- `Dockerfile.agent-runtime`、`Dockerfile.node-agent-runtime`
- `.github/workflows/images.yml` 中 agent-runtime 镜像构建项
- `config/samples/travel/demo/`（node-runtime 全接线演示；ox-alpha-model
  .yaml 归档后随目录删除）
- `docs/quickstart-custom-agent.md`（整篇围绕 cmd/agent-runtime 展开，
  前提消失；删除，待 pi runtime 落地后重写）

## 引用清理（改文字，不删文件）

- `README.md`（140、159、171、196-197 行附近）：移除 agent-runtime 作为
  fixture 的叙述与 `go run ./cmd/agent-runtime` 示例；watch 语义指向
  runtime-protocol.md §6 伪代码。
- `docs/usage.md`（226、237、374-379、396、407、426、806、815 行附近）：
  同上；repo 布局清单去掉 `cmd/agent-runtime`；"verification fixtures"
  注记收缩为 example-mcp / script-mcp。
- `docs/runtime-protocol.md`（11、270 行附近）：删除"参考实现是
  cmd/agent-runtime"的指向；SDK 仍是客户端参考，协议文档独立成立。
- `config/samples/travel/policy.yaml`：注释里的 `go run ./cmd/agent-runtime`
  改为指向协议文档。
- 今日 secret-env spec（2026-09-03-secret-env-injection-design.md）同步：
  "SDK / 参考 runtime / node-agent-runtime 改读 env" 一节改为 "runtime
  实现已删除，env-first 由 pi runtime 首个实现；SDK 仍保留 env 字段与
  回退读取"；验证项 4（参考 runtime 取值）移除。

## 不删

- `cmd/example-mcp`、`cmd/script-mcp`（MCP server，非 runtime）
- `config/samples/travel/` 其余样例（travel-assistant 用 coding-agent
  镜像，travel-agent 无 runtime 引用）
- `Dockerfile.coding-agent`（opencode，现行生产路径）
- `docs/adapter-protocol.md` 的示例 URL（泛指服务名，非引用）

## 验证

1. `go build ./...`、`go vet ./...`、`go test ./...` 通过（无残留 import）。
2. `grep -r "agent-runtime\|node-agent-runtime"` 除历史 spec 外无残留。
3. images.yml 语法有效（`actionlint` 或 YAML 解析）。

## 测试

- envtest 不受影响（test/e2e 未引用被删镜像）；无需新增测试。
