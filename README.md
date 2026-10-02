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

为路径 `{id}` 指定的数据湖表登记一个快照；表不存在时自动建表。请求体：

```json
{"snapshotId":"snap-1","fileCount":12,"retentionDays":7,"snapshotTime":"2026-09-01T10:00:00Z"}
```

- `snapshotId`：非空字符串，全局唯一；重复时返回 409 `snapshot_exists`
- `fileCount`：非负整数
- `retentionDays`：正整数；非正时返回 400 `invalid_retention_days`
- `snapshotTime`：RFC3339 时间

成功返回 201 与登记后的快照。缺字段、解析或类型错误返回 400 `invalid_request`。

### `GET /api/v1/lake-tables/{id}/expired`

列出表中 `snapshotTime + retentionDays` 早于请求时间且尚未清理的快照，按快照时间由旧到新返回；无结果时返回空数组 `[]`。表不存在返回 404 `table_not_found`。

### `POST /api/v1/maintenance`

对表内一批已过期且未清理的快照执行清理。请求体：

```json
{"tableId":"tbl-1","snapshotIds":["snap-1","snap-2"]}
```

成功返回 201，包含 `taskId`、`tableId`、`snapshotIds`、`status`（`completed`）与 `createdAt`，并在单个事务内按快照时间由旧到新标记清理，任一步失败整体回滚。

- `snapshotIds` 缺失或为空返回 400 `empty_snapshot_ids`
- 表不存在返回 404 `table_not_found`，快照不存在返回 404 `snapshot_not_found`
- 快照不属于该表或未过期返回 409 `snapshot_not_eligible`
- 快照已有成功任务返回 409 `snapshot_already_scheduled`

### `GET /api/v1/maintenance/{taskId}`

返回任务的 `taskId`、`tableId`、`snapshotIds`、`status` 与创建时间 `createdAt`。任务不存在返回 404 `task_not_found`。

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。

未匹配的路径返回 404 `route_not_found`。
