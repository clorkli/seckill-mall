# Go-Seckill-Mall

基于 Go 的秒杀微服务项目，核心目标是应对高并发下单场景，避免超卖，同时兼顾性能和最终一致性。
同时接入 OpenTelemetry + Jaeger 做链路追踪，Prometheus + Grafana 做监控。

## 项目定位

这不是单体架构，而是微服务架构（准确说是单仓库多服务，Monorepo + Microservices）：

- API Gateway 独立进程，负责 HTTP 入口、鉴权、限流
- Product Service 独立进程，负责商品查询和库存扣减
- Order Service 独立进程，负责下单编排和消息投递
- MQ Consumer 独立进程，负责异步消费并写入 MySQL

服务间使用 gRPC 调用，地址通过 etcd 动态发现。

## 整体架构

Client -> API Gateway (Gin + Sentinel + JWT)
-> gRPC -> Order Service
-> gRPC -> Product Service (Redis + Lua 扣库存)
-> RabbitMQ
-> MQ Consumer
-> MySQL

## 一条下单请求到 MySQL 的完整流程

1. 用户调用 /login 获取 JWT
2. 用户携带 Bearer Token 调用 /order
3. Gateway 先经过 Sentinel 限流和 JWT 鉴权
4. Gateway 调用 OrderService.CreateOrder
5. Order Service 调用 ProductService.DeductStock 扣 Redis 库存
6. 扣减成功后查询商品价格，生成订单号和订单消息
7. Order Service 将订单消息发送到 RabbitMQ
8. 如果 MQ 发送失败，Order Service 触发 RollbackStock 回滚库存
9. MQ Consumer 消费订单消息，写入 MySQL orders
10. 成功则 Ack；失败则 Nack，进入死信队列等待处理

## 环境准备

1. 本地安装 Docker 和 Docker Compose
2. 启动中间件：

docker-compose up -d

3. 准备 MySQL 数据库和表结构（见下方 MySQL 初始化）
4. 设置环境变量（必须）

export SECKILL_JWT_SECRET="replace-with-strong-secret"
export SECKILL_MYSQL_DSN="root:123456@tcp(127.0.0.1:3306)/seckill?charset=utf8mb4&parseTime=True&loc=Local"
export SECKILL_MQ_URL="amqp://guest:guest@127.0.0.1:5672/"

5. 按顺序启动服务：

go run product_service/main.go
go run order_service/main.go
go run mq_consumer/main.go
go run api_gateway/main.go

6. 压测：

go run stress_test/main.go

## MySQL 初始化

安装了 Database Client 后，还需要完成两件事：连接数据库 + 初始化表结构。

### 1) 准备 MySQL

方式 A（推荐，Docker）：

docker run -d --name seckill-mysql -p 3306:3306 -e MYSQL_ROOT_PASSWORD=123456 -e MYSQL_DATABASE=seckill mysql:8.0 --default-authentication-plugin=mysql_native_password

方式 B（本机已有 MySQL）：

确认可以连接 127.0.0.1:3306，且账号有建库建表权限。

### 2) 用 Database Client 建立连接

- Host: 127.0.0.1
- Port: 3306
- User: root
- Password: 123456
- Database: seckill

### 3) 执行初始化 SQL

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

## 一条订单的联调验证（推荐）

1. 调用登录接口拿 token
2. 带 Bearer token 调用 /order
3. 在 MySQL 查询 orders 表，确认订单落库

查询语句：

SELECT id, order_id, user_id, product_id, amount, status, created_at
FROM orders
ORDER BY id DESC
LIMIT 20;

## 配置与安全说明

- config 目录中的敏感项默认留空，避免明文提交
- 系统支持环境变量覆盖配置，优先级高于 yaml
- 当前已支持以下环境变量：
	- SECKILL_JWT_SECRET
	- SECKILL_MYSQL_DSN
	- SECKILL_MQ_URL

## 技术栈

- Go
- Gin
- gRPC + Protobuf
- etcd
- Redis + Lua
- RabbitMQ + DLQ
- MySQL + GORM
- OpenTelemetry + Jaeger
- Prometheus + Grafana
- Sentinel