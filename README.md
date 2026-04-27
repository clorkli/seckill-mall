# Go-Seckill-Mall

基于 Go 的秒杀微服务项目，目标是模拟高并发下单场景，并围绕库存防超卖、异步下单、最终一致性、链路追踪和监控逐步完善工程能力。

当前项目采用单仓库多服务模式：

- API Gateway：HTTP 入口，负责登录、JWT 鉴权、Sentinel 限流和请求转发。
- Product Service：商品查询、Redis Lua 原子扣库存、限购记录、库存回滚。
- Order Service：下单编排，调用商品服务扣库存，并同事务写入排队中订单和 Outbox 事件。
- Outbox Worker：扫描待投递事件，可靠发布 RabbitMQ，并处理重试和最终补偿。
- MQ Consumer：消费订单消息，使用 MySQL 事务推进订单状态并同步扣减 `product.stock`。
- DLQ Consumer：消费死信队列，按订单状态补偿 Redis 库存和用户购买记录，并标记失败订单。
- Common：公共配置、JWT 工具、链路追踪、protobuf 生成代码。

## 当前状态

项目当前主链路已经具备：

- 使用 gRPC + etcd 完成服务发现与服务间调用。
- 使用 Redis + Lua 原子扣减秒杀库存，避免并发下重复读写导致超卖。
- 支持用户限购记录，防止同一用户超过配置数量购买。
- 对非法购买数量做了多层校验，`count <= 0` 会在 Gateway、Order Service、Product Service 被拒绝。
- 已支持异步订单状态查询，订单会经历 `排队中(status=0)`、`已成功(status=1)`、`已失败(status=2)`。
- Gateway 提供 `GET /order/:order_id`，用户可查询自己的异步订单处理结果。
- Order Service 会在同一个 MySQL 事务中写入排队中订单和 `outbox_events` 待投递事件。
- Outbox Worker 会扫描待投递事件并可靠发布 MQ，超过最大重试后补偿 Redis 并标记订单失败。
- RabbitMQ 主队列配置死信交换机和死信队列，消费失败消息会进入 DLQ。
- DLQ Consumer 会消费死信队列，若订单不是成功状态，则执行 Redis 补偿并将订单标记为失败。
- Outbox Worker 发布订单消息时已启用 publisher confirm、mandatory 路由检查和消息持久化。
- Outbox Worker 发送 MQ 失败时会重建 RabbitMQ 连接和 Channel，并按 `outbox_events.retry_count` 延迟重试。
- MQ Consumer 和 DLQ Consumer 已支持 RabbitMQ connection/channel 断开后的自动重连和重新消费。
- 已为 Outbox 投递、主队列消费、死信补偿、Ack/Nack、重连、积压和处理耗时增加 Prometheus 业务指标。
- 已通过 RabbitMQ headers 传播 OpenTelemetry trace context，支持异步 MQ 发布、消费和死信补偿链路在 Jaeger 中关联。
- MQ Consumer 在 MySQL 事务中推进订单状态并扣减 `product.stock`，让 MySQL 成为最终库存账本。
- Redis 已开启 AOF，并通过 Docker volume 持久化 `/data`，降低容器重启后的库存丢失风险。
- 接入 OpenTelemetry + Jaeger 做链路追踪。
- 接入 Prometheus + Grafana 做基础监控。

## 整体架构

```text
Client
  -> API Gateway (Gin + JWT + Sentinel + Prometheus + OpenTelemetry)
  -> gRPC / etcd
  -> Order Service
  -> gRPC / etcd
  -> Product Service (Redis + Lua)
  -> Order Service
  -> MySQL (orders: pending + outbox_events: pending)
  -> Outbox Worker
  -> RabbitMQ
  -> MQ Consumer
  -> MySQL (orders: success + product.stock)

失败消息:
RabbitMQ dead_queue
  -> DLQ Consumer
  -> MySQL 查询订单状态
  -> Redis 补偿库存和用户购买记录
  -> MySQL (orders: failed)
```

## 下单流程

1. 用户调用 `POST /login` 获取 JWT。
2. 用户携带 `Bearer Token` 调用 `POST /order`。
3. Gateway 执行 Sentinel 限流、JWT 鉴权和参数校验。
4. Gateway 调用 `OrderService.CreateOrder`。
5. Order Service 校验购买数量，并调用 `ProductService.DeductStock`。
6. Product Service 使用 Redis Lua 原子判断库存、限购记录，并扣减 Redis 库存。
7. Order Service 查询商品价格，生成订单号，并写入 `orders.status = 0` 的排队中订单。
8. Order Service 在同一个 MySQL 事务中写入 `outbox_events.status = 0` 的待投递事件，事件中包含订单消息和 trace headers。
9. Outbox Worker 扫描待投递事件，将订单消息投递到 RabbitMQ，消息中包含 `order_id`、`user_id`、`product_id`、`count` 和 `amount`。
10. MQ Consumer 消费订单消息，校验消息格式和 `count`。
11. MQ Consumer 开启 MySQL 事务，锁定排队中订单，扣减 `product.stock`，并把订单标记为成功。
12. 事务成功则 Ack；失败则 Nack 且不重回队列，消息进入死信队列。
13. DLQ Consumer 消费死信消息，若订单不是成功状态，则补偿 Redis 库存和用户购买记录，并把订单标记为失败。

## 库存一致性设计

当前库存采用两层模型：

- Redis：高并发入口库存，负责快速扣减、限购判断和流量削峰。
- MySQL：最终库存账本，`product.stock` 在订单最终落库时同步扣减。

正常情况下：

```text
Redis 扣减成功
-> Order Service 同事务写入 Pending 订单和 Outbox Pending 事件
-> Outbox Worker 投递 MQ 成功
-> Outbox 事件更新为 Sent
-> Consumer 事务扣 MySQL product.stock
-> Consumer 事务更新订单为 Success
-> Ack
```

如果 Order Service 写入 Outbox 后立刻崩溃：

```text
Redis 已扣减
-> Pending 订单和 Outbox Pending 事件已写入
-> Outbox Worker 后台扫描 Pending 事件
-> Outbox Worker 投递 MQ
-> 投递成功后 Outbox 事件更新为 Sent
```

如果 Outbox Worker 达到最大重试仍无法投递：

```text
Redis 已扣减
-> Pending 订单和 Outbox Pending 事件已写入
-> Outbox Worker 多次投递失败
-> Redis 幂等补偿
-> Redis 库存恢复
-> Redis 用户购买记录恢复
-> 订单更新为 Failed
-> Outbox 事件更新为 Failed
```

当前 MQ 投递已由 Outbox Worker 完全接管。Order Service 不再直接连接 RabbitMQ，请求链路只负责 Redis 扣减和 MySQL 事务写入。只要 `orders` 和 `outbox_events` 提交成功，即使 Order Service 随后崩溃，Worker 也能继续投递未发送事件。

如果 Consumer 落库失败：

```text
消息进入死信队列
-> DLQ Consumer 查询 MySQL 订单状态
-> 订单不是 Success 则补偿 Redis 库存和用户购买记录
-> Pending 订单更新为 Failed
-> 补偿成功后 Ack 死信消息
```

DLQ Consumer 的 Redis 补偿使用 `order:rollback:{order_id}` 作为回滚标记，避免同一条死信消息重复投递时多次增加 Redis 库存。

Redis 重启或数据丢失后，Product Service 会从 MySQL 的 `product.stock` 预热库存。由于 Consumer 已同步扣减 MySQL 库存，相比早期版本，跨重启后重新预热导致超卖的风险已经明显降低。

## 环境准备

1. 安装 Docker 和 Docker Compose。
2. 启动 MySQL 和中间件：

```bash
docker compose up -d
```

3. 首次启动时，MySQL 会自动执行 `deploy/mysql/init.sql`，创建 `seckill` 库、核心表和一条测试商品。
4. 设置必要环境变量：

```bash
export SECKILL_JWT_SECRET="replace-with-strong-secret"
export SECKILL_MYSQL_DSN="root:123456@tcp(127.0.0.1:3306)/seckill?charset=utf8mb4&parseTime=True&loc=Local"
export SECKILL_MQ_URL="amqp://guest:guest@127.0.0.1:5672/"
```

5. 按顺序启动服务：

```bash
go run product_service/main.go
go run order_service/main.go
go run outbox_worker/main.go
go run mq_consumer/main.go
go run dlq_consumer/main.go
go run api_gateway/main.go
```

6. 可选：运行压测脚本：

```bash
go run stress_test/main.go
```

## MySQL 初始化

项目已在 `docker-compose.yaml` 中内置 MySQL 8.0，初始化 SQL 位于 `deploy/mysql/init.sql`。

```bash
docker compose up -d mysql
```

MySQL 容器第一次创建 `mysql_data` volume 时，会自动执行 `deploy/mysql/init.sql`，创建：

- `product`：商品和最终库存账本。
- `orders`：异步订单状态。
- `outbox_events`：Outbox 可靠投递事件。
- 测试商品：`id=1`，`stock=100`。

如果你修改了初始化 SQL，并且想在本地重新执行整套初始化，可以删除 MySQL volume 后重启：

```bash
docker compose down
docker volume rm seckill-mall_mysql_data
docker compose up -d mysql
```

注意：删除 volume 会清空本地 MySQL 数据。

也可以手动执行初始化脚本：

```bash
docker exec -i seckill-mysql mysql -uroot -p123456 seckill < deploy/mysql/init.sql
```

如果你已有旧版 `orders` 表，需要先补充订单状态查询所需字段：

```sql
ALTER TABLE orders
ADD COLUMN count INT NOT NULL DEFAULT 1 AFTER product_id,
ADD COLUMN fail_reason VARCHAR(255) DEFAULT '' AFTER status,
ADD COLUMN updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP AFTER created_at,
ADD INDEX idx_user_id (user_id);
```

同时需要新增 Outbox 事件表：

```sql
CREATE TABLE IF NOT EXISTS outbox_events (
	id BIGINT UNSIGNED PRIMARY KEY AUTO_INCREMENT,
	event_id VARCHAR(64) NOT NULL,
	aggregate_type VARCHAR(32) NOT NULL,
	aggregate_id VARCHAR(64) NOT NULL,
	event_type VARCHAR(64) NOT NULL,
	payload JSON NOT NULL,
	headers JSON,
	status INT NOT NULL DEFAULT 0,
	retry_count INT NOT NULL DEFAULT 0,
	next_retry_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	last_error VARCHAR(255) DEFAULT '',
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
	UNIQUE KEY uk_event_id (event_id),
	KEY idx_status_next_retry (status, next_retry_at),
	KEY idx_aggregate_id (aggregate_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

如果你已经创建过旧版 `outbox_events` 表，需要补充 trace headers 字段：

```sql
ALTER TABLE outbox_events
ADD COLUMN headers JSON AFTER payload;
```

## 联调验证

### 1. 登录获取 Token

```bash
curl -X POST http://127.0.0.1:8080/login \
  -H "Content-Type: application/json" \
  -d '{"user_id": 1001}'
```

### 2. 携带 Token 下单

```bash
curl -X POST http://127.0.0.1:8080/order \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <your-token>" \
  -d '{"product_id": 1, "count": 1}'
```

### 3. 通过 HTTP 查询订单状态

```bash
curl http://127.0.0.1:8080/order/<order_id> \
  -H "Authorization: Bearer <your-token>"
```

订单状态含义：

- `status = 0`：排队中，订单已受理，等待 MQ Consumer 最终处理。
- `status = 1`：已成功，订单已落库并同步扣减 MySQL `product.stock`。
- `status = 2`：已失败，Redis 库存和用户购买记录已尝试补偿。

### 4. 查询订单和库存

```sql
SELECT id, order_id, user_id, product_id, count, amount, status, fail_reason, created_at, updated_at
FROM orders
ORDER BY id DESC
LIMIT 20;

SELECT id, name, stock
FROM product
WHERE id = 1;
```

如果订单成功消费，`orders.status` 会从 `0` 更新为 `1`，`product.stock` 会同步减少。

## 配置说明

配置文件位于 `config/` 目录，不同服务使用不同配置文件：

- `config/gateway.yaml`
- `config/product.yaml`
- `config/order.yaml`
- `config/mq.yaml`

敏感配置建议通过环境变量注入：

- `SECKILL_JWT_SECRET`
- `SECKILL_MYSQL_DSN`
- `SECKILL_MQ_URL`
- `SECKILL_DLQ_METRICS_PORT`
- `SECKILL_OUTBOX_METRICS_PORT`

## 监控与追踪

- Jaeger UI：`http://127.0.0.1:16686`
- Prometheus：`http://127.0.0.1:9090`
- Grafana：`http://127.0.0.1:3000`
- Gateway metrics：`http://127.0.0.1:8080/metrics`
- Product metrics：`http://127.0.0.1:9091/metrics`
- Order metrics：`http://127.0.0.1:9092/metrics`
- MQ Consumer metrics：`http://127.0.0.1:9093/metrics`
- DLQ Consumer metrics：`http://127.0.0.1:9094/metrics`
- Outbox Worker metrics：`http://127.0.0.1:9095/metrics`
- RabbitMQ broker metrics：`http://127.0.0.1:15692/metrics`

DLQ Consumer 的 metrics 端口默认是 `9094`，可以通过 `SECKILL_DLQ_METRICS_PORT` 覆盖。
Outbox Worker 的 metrics 端口默认是 `9095`，可以通过 `SECKILL_OUTBOX_METRICS_PORT` 覆盖。

RabbitMQ broker metrics 由 `rabbitmq_prometheus` 插件提供，Prometheus 会通过 Docker 网络抓取 `rabbitmq:15692`。业务侧 MQ 指标用于观察 Outbox 积压、发布、消费、补偿和重连；broker 侧指标用于观察队列积压、连接数、channel 数、consumer 数、消息 ready/unacked 等 RabbitMQ 自身状态。

Outbox Worker 当前暴露的核心指标包括：

- `seckill_outbox_pending_events`：当前待投递 Outbox 事件数量，持续升高说明 Worker 发布慢、RabbitMQ 异常或 Consumer 链路阻塞。
- `seckill_outbox_scan_total`：Outbox 扫描次数，按 `result` 区分成功和失败。
- `seckill_outbox_claimed_total`：被 Worker 领取处理的事件总数。
- `seckill_outbox_publish_total`：RabbitMQ 发布尝试次数，按 `result` 区分成功和失败。
- `seckill_outbox_publish_duration_seconds`：Outbox 发布 RabbitMQ 的耗时分布。
- `seckill_outbox_process_duration_seconds`：单条 Outbox 事件端到端处理耗时，按处理结果分类。
- `seckill_outbox_retry_total`：Outbox 安排重试的次数，按 `reason` 区分发布失败、补偿失败或状态更新失败。
- `seckill_outbox_compensation_total`：达到最大投递重试后执行 Redis 最终补偿的结果。
- `seckill_outbox_reconnect_total`：Outbox Worker RabbitMQ 发布连接重建次数。

MQ 异步链路会通过 RabbitMQ headers 传递 OpenTelemetry trace context。Order Service 会把 `traceparent`/`baggage` 写入 `outbox_events.headers`，Outbox Worker 发布 RabbitMQ 消息时恢复这些 headers，MQ Consumer 和 DLQ Consumer 消费消息时提取上下文并创建消费 span，因此 Jaeger 中可以关联下单请求、Outbox 投递、异步落库和死信补偿链路。

## 当前注意事项

- `mq_consumer` 对旧格式 MQ 消息不兼容。旧消息没有 `count` 字段，会被识别为非法消息并进入死信队列。
- `dlq_consumer` 对旧格式或字段缺失的死信消息无法自动补偿，会记录日志并确认消息，避免毒丸消息阻塞队列。
- Order Service 会写入排队中订单，已有旧表需要先执行 `orders` 表字段升级 SQL，否则会因为缺少 `count`、`fail_reason` 或 `updated_at` 导致写入失败。
- Outbox Worker 已完全接管 MQ 投递，Order Service 不再直接依赖 RabbitMQ；启动服务时需要确保 `outbox_worker` 正常运行，否则订单会停留在 `status = 0` 排队中。
- Product Service 在 `debug` 模式下会启用 `/dev/reset`，该接口会清空 Redis 和 `orders` 表，但当前不会自动恢复 `product.stock` 到初始值。
- DLQ Consumer 已支持常见落库失败后的 Redis 补偿，但对用户购买记录小于回滚数量等异常状态仍需要人工核查日志。
- RabbitMQ 生产者侧已启用 publisher confirm、persistent message、mandatory return 和失败重连重试；Consumer 侧已支持连接断开后自动重连。
- 当前已接入业务侧 MQ/Outbox 指标和 RabbitMQ broker 指标，但还没有内置 Grafana dashboard 与 Prometheus alert 规则。
- etcd 注册地址当前偏本地开发场景，服务地址仍以 `127.0.0.1` 为主，多机或容器化部署需要调整。

## 继续优化方向

建议按优先级继续推进：

1. 增加 Grafana dashboard 和 Prometheus alert：展示 MQ publish、consume、DLQ 补偿、重连、队列积压，并配置失败率和积压告警。
2. 增强 Outbox 告警和仪表盘：围绕待投递积压、发布失败率、重试次数、最终补偿失败和重连次数配置 Prometheus alert 与 Grafana dashboard。
3. 修复开发重置能力：让 `/dev/reset` 同步恢复 `product.stock` 到测试初始库存，或改成显式传入重置库存。
4. 扩展订单状态机：增加已取消、超时关闭、人工核查等状态，并记录状态流转历史。
5. 改善服务注册：etcd 注册地址改为可配置，支持 Docker、WSL、多机部署场景。
6. 增加自动化测试：补充 Redis Lua、库存回滚、Consumer 幂等、MySQL 事务扣库存、DLQ 补偿、MQ 发布确认、消费端重连和 MQ trace propagation 等核心测试。
7. 增加数据库迁移并完善安全边界：引入 migration 工具，关闭生产环境 `/dev/reset`，JWT secret 强度校验，敏感日志脱敏。

## 技术栈

- Go
- Gin
- gRPC + Protobuf
- etcd
- Redis + Lua + AOF
- RabbitMQ + DLQ
- MySQL + GORM
- OpenTelemetry + Jaeger
- Prometheus + Grafana
- Sentinel
