# Go-Seckill-Mall

基于 Go 的秒杀微服务项目，目标是模拟高并发下单场景，并围绕库存防超卖、异步下单、最终一致性、链路追踪和监控逐步完善工程能力。

当前项目采用单仓库多服务模式：

- API Gateway：HTTP 入口，负责登录、JWT 鉴权、Sentinel 限流和请求转发。
- Product Service：商品查询、Redis Lua 原子扣库存、限购记录、库存回滚。
- Order Service：下单编排，调用商品服务扣库存，生成订单消息并投递 RabbitMQ。
- MQ Consumer：消费订单消息，使用 MySQL 事务写入订单并同步扣减 `product.stock`。
- DLQ Consumer：消费死信队列，确认订单未落库后补偿 Redis 库存和用户购买记录。
- Common：公共配置、JWT 工具、链路追踪、protobuf 生成代码。

## 当前状态

项目当前主链路已经具备：

- 使用 gRPC + etcd 完成服务发现与服务间调用。
- 使用 Redis + Lua 原子扣减秒杀库存，避免并发下重复读写导致超卖。
- 支持用户限购记录，防止同一用户超过配置数量购买。
- 对非法购买数量做了多层校验，`count <= 0` 会在 Gateway、Order Service、Product Service 被拒绝。
- MQ 投递失败时会调用 `RollbackStock`，同时回滚 Redis 库存和用户购买记录。
- RabbitMQ 主队列配置死信交换机和死信队列，消费失败消息会进入 DLQ。
- DLQ Consumer 会消费死信队列，先检查 MySQL 订单是否已存在，未落库才执行 Redis 补偿。
- Order Service 发布订单消息时已启用 publisher confirm、mandatory 路由检查和消息持久化。
- Order Service 发送 MQ 失败时会重建 RabbitMQ 连接和 Channel，并自动重试一次。
- MQ Consumer 和 DLQ Consumer 已支持 RabbitMQ connection/channel 断开后的自动重连和重新消费。
- MQ Consumer 在 MySQL 事务中写入 `orders` 并扣减 `product.stock`，让 MySQL 成为最终库存账本。
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
  -> RabbitMQ
  -> MQ Consumer
  -> MySQL (orders + product.stock)

失败消息:
RabbitMQ dead_queue
  -> DLQ Consumer
  -> MySQL 查询订单是否已落库
  -> Redis 补偿库存和用户购买记录
```

## 下单流程

1. 用户调用 `POST /login` 获取 JWT。
2. 用户携带 `Bearer Token` 调用 `POST /order`。
3. Gateway 执行 Sentinel 限流、JWT 鉴权和参数校验。
4. Gateway 调用 `OrderService.CreateOrder`。
5. Order Service 校验购买数量，并调用 `ProductService.DeductStock`。
6. Product Service 使用 Redis Lua 原子判断库存、限购记录，并扣减 Redis 库存。
7. Order Service 查询商品价格，生成订单号和 MQ 消息。
8. Order Service 将订单消息投递到 RabbitMQ，消息中包含 `order_id`、`user_id`、`product_id`、`count` 和 `amount`。
9. 如果 MQ 投递失败，Order Service 调用 `RollbackStock`，恢复 Redis 库存和用户购买记录。
10. MQ Consumer 消费订单消息，校验消息格式和 `count`。
11. MQ Consumer 开启 MySQL 事务，写入 `orders` 并扣减 `product.stock`。
12. 事务成功则 Ack；失败则 Nack 且不重回队列，消息进入死信队列。
13. DLQ Consumer 消费死信消息，若 MySQL 中不存在该订单，则补偿 Redis 库存和用户购买记录。

## 库存一致性设计

当前库存采用两层模型：

- Redis：高并发入口库存，负责快速扣减、限购判断和流量削峰。
- MySQL：最终库存账本，`product.stock` 在订单最终落库时同步扣减。

正常情况下：

```text
Redis 扣减成功
-> MQ 投递成功
-> Consumer 事务写订单
-> Consumer 事务扣 MySQL product.stock
-> Ack
```

如果 MQ 投递失败：

```text
Redis 已扣减
-> MQ 投递失败
-> RollbackStock
-> Redis 库存恢复
-> Redis 用户购买记录恢复
```

如果 Consumer 落库失败：

```text
消息进入死信队列
-> DLQ Consumer 查询 MySQL 是否已有订单
-> 订单不存在则补偿 Redis 库存和用户购买记录
-> 补偿成功后 Ack 死信消息
```

DLQ Consumer 的 Redis 补偿使用 `order:rollback:{order_id}` 作为回滚标记，避免同一条死信消息重复投递时多次增加 Redis 库存。

Redis 重启或数据丢失后，Product Service 会从 MySQL 的 `product.stock` 预热库存。由于 Consumer 已同步扣减 MySQL 库存，相比早期版本，跨重启后重新预热导致超卖的风险已经明显降低。

## 环境准备

1. 安装 Docker 和 Docker Compose。
2. 启动中间件：

```bash
docker compose up -d
```

3. 准备 MySQL 数据库和表结构。
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
go run mq_consumer/main.go
go run dlq_consumer/main.go
go run api_gateway/main.go
```

6. 可选：运行压测脚本：

```bash
go run stress_test/main.go
```

## MySQL 初始化

### 1. 准备 MySQL

方式 A：使用 Docker 启动 MySQL。

```bash
docker run -d --name seckill-mysql \
  -p 3306:3306 \
  -e MYSQL_ROOT_PASSWORD=123456 \
  -e MYSQL_DATABASE=seckill \
  mysql:8.0 --default-authentication-plugin=mysql_native_password
```

方式 B：使用本机已有 MySQL。

确认可以连接 `127.0.0.1:3306`，且账号有建库建表权限。

### 2. 执行初始化 SQL

```sql
CREATE DATABASE IF NOT EXISTS seckill DEFAULT CHARACTER SET utf8mb4;
USE seckill;

CREATE TABLE IF NOT EXISTS product (
	id BIGINT PRIMARY KEY AUTO_INCREMENT,
	name VARCHAR(255) NOT NULL,
	price DECIMAL(10,2) NOT NULL DEFAULT 0.00,
	stock INT NOT NULL DEFAULT 0,
	description VARCHAR(255) DEFAULT ''
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS orders (
	id BIGINT UNSIGNED PRIMARY KEY AUTO_INCREMENT,
	order_id VARCHAR(64) NOT NULL,
	user_id BIGINT NOT NULL,
	product_id BIGINT NOT NULL,
	amount DECIMAL(10,2) NOT NULL,
	status INT NOT NULL DEFAULT 0,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE KEY uk_order_id (order_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

INSERT INTO product (name, price, stock, description)
VALUES ('iPhone 15', 6999.00, 100, '秒杀测试商品');
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

### 3. 查询订单和库存

```sql
SELECT id, order_id, user_id, product_id, amount, status, created_at
FROM orders
ORDER BY id DESC
LIMIT 20;

SELECT id, name, stock
FROM product
WHERE id = 1;
```

如果订单成功消费，`orders` 会新增记录，`product.stock` 会同步减少。

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

## 监控与追踪

- Jaeger UI：`http://127.0.0.1:16686`
- Prometheus：`http://127.0.0.1:9090`
- Grafana：`http://127.0.0.1:3000`
- Gateway metrics：`http://127.0.0.1:8080/metrics`
- Product metrics：`http://127.0.0.1:9091/metrics`
- Order metrics：`http://127.0.0.1:9092/metrics`

## 当前注意事项

- `mq_consumer` 对旧格式 MQ 消息不兼容。旧消息没有 `count` 字段，会被识别为非法消息并进入死信队列。
- `dlq_consumer` 对旧格式或字段缺失的死信消息无法自动补偿，会记录日志并确认消息，避免毒丸消息阻塞队列。
- Product Service 在 `debug` 模式下会启用 `/dev/reset`，该接口会清空 Redis 和 `orders` 表，但当前不会自动恢复 `product.stock` 到初始值。
- DLQ Consumer 已支持常见落库失败后的 Redis 补偿，但对用户购买记录小于回滚数量等异常状态仍需要人工核查日志。
- RabbitMQ 生产者侧已启用 publisher confirm、persistent message、mandatory return 和失败重连重试；Consumer 侧已支持连接断开后自动重连。
- etcd 注册地址当前偏本地开发场景，服务地址仍以 `127.0.0.1` 为主，多机或容器化部署需要调整。

## 继续优化方向

建议按优先级继续推进：

1. 完善 MQ 可观测性：增加 publish confirm 成功率、publish retry 次数、return message、Consumer Nack、DLQ 补偿结果、Consumer reconnect 次数等指标和告警。
2. 引入 Outbox 模式：进一步降低“Redis 已扣减但 MQ 确认状态不明”这类分布式边界风险。
3. 修复开发重置能力：让 `/dev/reset` 同步恢复 `product.stock` 到测试初始库存，或改成显式传入重置库存。
4. 抽离订单状态：引入订单状态机，例如排队中、已创建、失败、已取消，避免只依赖 MQ 成功与否判断订单结果。
5. 增加查询接口：增加订单查询接口，用户下单后可以查询异步处理结果。
6. 改善服务注册：etcd 注册地址改为可配置，支持 Docker、WSL、多机部署场景。
7. 增加自动化测试：补充 Redis Lua、库存回滚、Consumer 幂等、MySQL 事务扣库存、DLQ 补偿、MQ 发布确认和消费端重连等核心测试。
8. 增加数据库迁移：引入 migration 工具管理 schema，避免 README 中手工 SQL 与代码模型漂移。
9. 完善配置加载与安全边界：支持更多环境变量覆盖，关闭生产环境 `/dev/reset`，JWT secret 强度校验，敏感日志脱敏。

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
