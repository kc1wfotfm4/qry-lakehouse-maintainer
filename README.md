# qry-lakehouse-maintainer

把数据湖表的快照、清单文件、保留周期与维护任务记录成可查询的服务，支持按表评估过期快照、提交清理与小文件合并并跟踪维护进度。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `qry-lakehouse-maintainer.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### `POST /api/v1/lake-tables/{id}/snapshots`

为路径 `{id}` 的表登记一个快照；表不存在时自动建表。请求体：

```json
{"snapshotId":"s-1","fileCount":12,"retentionDays":30,"snapshotTime":"2026-01-01T00:00:00Z"}
```

- `snapshotId`：非空字符串，全局唯一；重复返回 409 `snapshot_exists`
- `fileCount`：非负整数
- `retentionDays`：正整数；非正返回 400 `invalid_retention_days`
- `snapshotTime`：RFC3339 时间

成功返回 201 与登记的快照。

### `GET /api/v1/lake-tables/{id}/expired`

列出该表已过期（`snapshotTime + retentionDays` 早于请求时间）且未清理的快照，按快照时间由旧到新返回；无结果时返回 `[]`。表不存在返回 404 `table_not_found`。

### `POST /api/v1/maintenance`

为表内已过期且未清理的快照提交清理任务。请求体：

```json
{"tableId":"t-1","snapshotIds":["s-1","s-2"]}
```

清理在单个事务内按快照时间由旧到新标记，任一步失败整体回滚。成功返回 201：

```json
{"taskId":"...","tableId":"t-1","snapshotIds":["s-1","s-2"],"status":"completed","createdAt":"..."}
```

`snapshotIds` 为空数组返回 400 `empty_snapshot_ids`；表或快照不存在分别返回 404 `table_not_found`、`snapshot_not_found`；快照不属于该表或未过期返回 409 `snapshot_not_eligible`；快照已有成功任务返回 409 `snapshot_already_scheduled`。

### `GET /api/v1/maintenance/{taskId}`

返回 `taskId`、`tableId`、`snapshotIds`、`status` 与 `createdAt`；任务不存在返回 404 `task_not_found`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 只描述业务原因，不包含 SQL、堆栈或文件路径。未匹配的路径返回 404 `route_not_found`；缺字段、JSON 解析或类型错误返回 400 `invalid_request`。
