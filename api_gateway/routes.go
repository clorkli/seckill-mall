package main

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	ginprometheus "github.com/zsais/go-gin-prometheus"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"seckill-mall/api_gateway/middleware"
	"seckill-mall/common/config"
	"seckill-mall/common/pb"
	"seckill-mall/common/utils"
)

func setupRouter(clients grpcClients) *gin.Engine {
	// 启动 Gin
	r := gin.Default()

	p := ginprometheus.NewPrometheus("gin") //添加Prometheus监控中间件
	p.Use(r)

	//添加Gin中间件，自动记录http请求
	r.Use(otelgin.Middleware("api-gateway"))

	registerRoutes(r, clients)
	return r
}

func registerRoutes(r *gin.Engine, clients grpcClients) {
	productClient := clients.product
	orderClient := clients.order

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

	// 接口: 查询订单
	r.GET("/order/:order_id", middleware.JWTAuth(), func(c *gin.Context) {
		userID, exists := c.Get("userID")
		if !exists {
			c.JSON(401, gin.H{"error": "未鉴权用户"})
			return
		}

		orderID := c.Param("order_id")
		if orderID == "" {
			c.JSON(400, gin.H{"error": "订单号不能为空"})
			return
		}

		resp, err := orderClient.GetOrder(c.Request.Context(), &pb.GetOrderRequest{
			OrderId: orderID,
			UserId:  userID.(int64),
		})
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		if !resp.Found {
			c.JSON(404, gin.H{
				"code":    404,
				"message": resp.Message,
			})
			return
		}

		c.JSON(200, gin.H{
			"code":    200,
			"message": "查询成功",
			"data":    resp,
		})
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
		if req.Count <= 0 {
			c.JSON(400, gin.H{"error": "购买数量必须大于0"})
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
			"message": "请求已受理",
			"data":    resp,
		})
	})
}
