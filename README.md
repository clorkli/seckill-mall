# Go-Seckill-Mall

基于 Go 的秒杀微服务示例工程，核心目标是应对高并发下单场景，避免超卖，同时兼顾性能和最终一致性。
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

3. 准备 MySQL 数据库和表结构（项目当前未包含 SQL 文件）
4. 按顺序启动服务：

go run product_service/main.go
go run order_service/main.go
go run mq_consumer/main.go
go run api_gateway/main.go

5. 压测：

go run stress_test/main.go

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