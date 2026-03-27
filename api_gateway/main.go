package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	ginprometheus "github.com/zsais/go-gin-prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	clientv3 "go.etcd.io/etcd/client/v3"
	resolver "go.etcd.io/etcd/client/v3/naming/resolver"

	"seckill-mall/common/config"
	"seckill-mall/common/pb"
	"seckill-mall/common/tracer"

	"seckill-mall/api_gateway/middleware"

	"seckill-mall/common/utils"

	sentinel "github.com/alibaba/sentinel-golang/api"
	"github.com/alibaba/sentinel-golang/core/flow"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
)

func initSentinel() {
	const (
		resourceName = "create_order"
		threshold    = float64(1000) //将限流阈值与日志输出绑定
	)

	// 初始化 Sentinel
	err := sentinel.InitDefault()
	if err != nil {
		log.Fatalf("初始化 Sentinel 失败: %v", err)
	}

	// 配置限流规则
	_, err = flow.LoadRules([]*flow.Rule{
		{
			Resource:               resourceName, // 资源名称
			TokenCalculateStrategy: flow.Direct,  // 直接计数
			ControlBehavior:        flow.Reject,  // 直接拒绝
			Threshold:              threshold,
			StatIntervalInMs:       1000, // 统计周期1秒
		},
	})
	if err != nil {
		log.Fatalf("加载限流规则失败: %v", err)
	}

	log.Printf("Sentinel限流规则已加载：%s 每秒最大请求数 %.0f", resourceName, threshold)
}

func main() {
	// 先加载配置
	config.InitConfig("gateway")

	//初始化链路追踪
	shutdown := tracer.InitTracer("api-gateway", "localhost:4318")
	defer shutdown(context.Background())

	// 初始化 Sentinel
	initSentinel()

	// 使用配置里的 Etcd 地址
	etcdAddr := config.Conf.Etcd.Addr

	// 初始化 Etcd 连接
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{etcdAddr}, // 使用配置变量
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatalf("连接 Etcd 失败: %v", err)
	}
	etcdResolver, err := resolver.NewBuilder(cli)
	if err != nil {
		log.Fatalf("创建解析器失败: %v", err)
	}

	// 连接【商品服务】
	connProduct, err := grpc.Dial(
		"etcd:///seckill/product",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithResolvers(etcdResolver),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
	)
	if err != nil {
		log.Fatalf("无法连接商品服务: %v", err)
	}
	productClient := pb.NewProductServiceClient(connProduct)

	// 连接【订单服务】
	connOrder, err := grpc.Dial(
		"etcd:///seckill/order",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithResolvers(etcdResolver),
		grpc.WithDefaultServiceConfig(`{"loadBalancingPolicy":"round_robin"}`),
	)
	if err != nil {
		log.Fatalf("无法连接订单服务: %v", err)
	}
	orderClient := pb.NewOrderServiceClient(connOrder)

	// 启动 Gin
	r := gin.Default()

	p := ginprometheus.NewPrometheus("gin") //添加Prometheus监控中间件
	p.Use(r)

	//添加Gin中间件，自动记录http请求
	r.Use(otelgin.Middleware("api-gateway"))

	//模拟登录接口
	r.POST("/login", func(c *gin.Context) {
		type LoginReq struct {
			UserID int64 `json:"user_id"`
		}
		var req LoginReq
		if err := c.ShouldBind(&req); err != nil {
			c.JSON(400, gin.H{"error": "参数错误"})
			return
		}
		expireStr := config.Conf.JWT.Expire

		//解析时间字符串
		expireDuration, err := time.ParseDuration(expireStr)
		if err != nil {
			expireDuration = 2 * time.Hour
		}

		// 生成Token
		token, err := utils.GenerateToken(req.UserID, expireDuration)

		if err != nil {
			c.JSON(500, gin.H{"error": "生成Token失败"})
			return
		}

		c.JSON(200, gin.H{
			"code":    200,
			"message": "登录成功",
			"token":   token,
			"expire":  expireStr,
		})
	})

	// 接口: 查询商品
	r.GET("/product/:id", func(c *gin.Context) {
		id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
		resp, err := productClient.GetProduct(c.Request.Context(), &pb.ProductRequest{ProductId: id})
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"data": resp})
	})

	// 接口: 下单
	r.POST("/order", middleware.SentinelLimit("create_order"), middleware.JWTAuth(), func(c *gin.Context) {

		//从Context中获取UserID，需要将Context里的interface{}类型断言为int64
		userID, exists := c.Get("userID")
		if !exists {
			c.JSON(401, gin.H{"error": "未鉴权用户"})
			return
		}

		var req struct {
			ProductID int64 `json:"product_id"`
			Count     int32 `json:"count"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "参数错误"})
			return
		}

		resp, err := orderClient.CreateOrder(c.Request.Context(), &pb.CreateOrderRequest{
			UserId:    userID.(int64),
			ProductId: req.ProductID,
			Count:     req.Count,
		})

		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		c.JSON(200, gin.H{
			"code":    200,
			"message": "下单成功",
			"data":    resp,
		})
	})

	port := config.Conf.Server.Port
	if port == "" {
		port = "8080"
	}

	addr := net.JoinHostPort("", port)
	fmt.Printf("=== API 网关已启动 (Port: %s) ===\n", addr)
	r.Run(addr)
}
