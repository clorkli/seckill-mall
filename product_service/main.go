package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"seckill-mall/common/config"
	"strconv"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/naming/endpoints"

	"seckill-mall/common/pb"

	// 引入 Redis 库
	"github.com/redis/go-redis/v9"

	"seckill-mall/common/tracer"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	SERVICE_NAME = "seckill/product"
)

// 定义 Lua 脚本
// KEYS[1]: 商品的 Redis Key (例如 product:stock:1)
// ARGV[1]: 要扣减的数量
const LUA_SCRIPT = `
-- 商品Key不存在（未预热/错误ID）
if redis.call("EXISTS", KEYS[1]) == 0 then
	return 0
end

-- 重复购买
local current_buy = tonumber(redis.call('hget', KEYS[2], ARGV[2])) or 0
local want_buy = tonumber(ARGV[1])
local limit = tonumber(ARGV[3])  -- 每人限购ARGV[3]件

if current_buy + want_buy > limit then
	return 3
end

local stock = tonumber(redis.call("GET", KEYS[1]))

-- 库存不足
if stock < want_buy then
	return 2
end

-- 扣减库存
redis.call("decrby", KEYS[1], want_buy)
redis.call("hincrby", KEYS[2], ARGV[2], want_buy) --记录用户购买行为
return 1
`

// 升级 DeductStock 接口，区分库存为零与商品不存在两种情况
func (s *server) DeductStock(ctx context.Context, req *pb.DeductStockRequest) (*pb.DeductStockResponse, error) {
	fmt.Printf("[Trace]扣减库存：用户%d, 商品%d, 数量%d\n", req.UserId, req.ProductId, req.Count)
	// 拼接 Key: product:stock:1
	stockKey := "product:stock:" + strconv.FormatInt(req.ProductId, 10)
	userSetKey := "product:users:" + strconv.FormatInt(req.ProductId, 10) //新增用户购买集合Key

	PurchaseLimit := config.Conf.Seckill.PurchaseLimit // 限购数据在配置文件中设置

	if PurchaseLimit <= 0 {
		PurchaseLimit = 1 // 预防限购未设置，默认每人限购1件
	}

	// 执行 Lua 脚本
	val, err := rdb.Eval(ctx, LUA_SCRIPT, []string{stockKey, userSetKey}, req.Count, req.UserId, PurchaseLimit).Int()

	if err != nil {
		log.Printf("❌ Redis执行异常: %v", err)
		return nil, err
	}

	// 根据 Lua 返回的状态码进行精准处理
	switch val {
	case 0: // 商品不存在
		log.Printf("拒绝扣减：商品 %d 未预热或不存在", req.ProductId)
		return &pb.DeductStockResponse{
			Success: false,
			Message: "商品不存在或未上架", //给出明确的错误提示
		}, nil
	case 2: // 库存不足
		log.Printf("拒绝扣减：商品 %d 库存不足", req.ProductId)
		return &pb.DeductStockResponse{
			Success: false,
			Message: "库存不足",
		}, nil
	case 1: // 成功
		fmt.Printf("扣减成功：用户%d买到了商品 %d \n", req.UserId, req.ProductId)
		return &pb.DeductStockResponse{Success: true, Message: "扣减成功"}, nil
	case 3: // 重复购买
		log.Printf("超过限购：用户 %d 试图购买商品 %d 一共%d件，限购%d 件", req.UserId, req.ProductId, req.Count, PurchaseLimit)
		return &pb.DeductStockResponse{
			Success: false,
			Message: "每人限购一件，您已购买过该商品，不能重复购买",
		}, nil
	default:
		return &pb.DeductStockResponse{Success: false, Message: "未知错误"}, nil
	}
}

// 实现 RollbackStock 接口
func (s *server) RollbackStock(ctx context.Context, req *pb.DeductStockRequest) (*pb.DeductStockResponse, error) {
	fmt.Printf("[Rollback]收到回滚请求：商品%d, 数量%d\n", req.ProductId, req.Count)

	key := "product:stock:" + strconv.FormatInt(req.ProductId, 10)

	//使用Redis的INCRBY原子操作回滚库存
	err := rdb.IncrBy(ctx, key, int64(req.Count)).Err()
	if err != nil {
		fmt.Printf("X! 回滚失败，CRITICAL ERROR：%v\n", err)
		return &pb.DeductStockResponse{Success: false, Message: "回滚失败: " + err.Error()}, nil
	}

	fmt.Printf("回滚成功，库存已恢复\n")
	return &pb.DeductStockResponse{Success: true, Message: "回滚成功"}, nil
}

// 数据库模型
type Product struct {
	ID          int64   `gorm:"primaryKey"`
	Name        string  `gorm:"type:varchar(255)"`
	Price       float32 `gorm:"type:decimal(10,2)"`
	Stock       int32   `gorm:"type:int"`
	Description string  `gorm:"type:varchar(255)"`
}

func (Product) TableName() string { return "product" }

var db *gorm.DB
var rdb *redis.Client // 全局 Redis 客户端

type server struct {
	pb.UnimplementedProductServiceServer
}

// GetProduct 实现
func (s *server) GetProduct(ctx context.Context, req *pb.ProductRequest) (*pb.ProductResponse, error) {
	fmt.Printf("[Trace]查询商品：%d\n", req.ProductId)

	var product Product
	if err := db.First(&product, req.ProductId).Error; err != nil {
		return nil, err
	}
	return &pb.ProductResponse{
		ProductId: product.ID, Name: product.Name, Price: product.Price,
	}, nil
}

// 初始化 Redis
func initRedis() {
	rdb = redis.NewClient(&redis.Options{
		Addr:     config.Conf.Redis.Addr,
		Password: config.Conf.Redis.Password,
		DB:       config.Conf.Redis.DB,
	})

	// 测试连接
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("连接 Redis 失败: %v", err)
	}
	fmt.Println("Redis 连接成功！")
}

// 新增：预热库存到 Redis
// 本来通常通过后台管理系统触发，这里简化为启动时自动加载
func preheatStock() {
	var products []Product
	db.Find(&products) // 查出所有商品

	for _, p := range products {
		key := "product:stock:" + strconv.FormatInt(p.ID, 10)

		// SetNX: 如果 Key 不存在才设置 (防止重启服务覆盖了已经扣减的库存)
		// 这里的 value 就是库存数
		err := rdb.SetNX(context.Background(), key, p.Stock, 0).Err()
		if err != nil {
			fmt.Printf("预热库存失败 %d: %v\n", p.ID, err)
		} else {
			fmt.Printf("🔥 库存已预热: %s => %d\n", key, p.Stock)
		}
	}
}

func RegisterEtcd(port string) {
	etcdAddr := config.Conf.Etcd.Addr
	myAddr := "127.0.0.1:" + port

	cli, _ := clientv3.New(clientv3.Config{Endpoints: []string{etcdAddr}})
	em, _ := endpoints.NewManager(cli, SERVICE_NAME)
	lease, _ := cli.Grant(context.TODO(), 10)

	em.AddEndpoint(context.TODO(), SERVICE_NAME+"/"+myAddr, endpoints.Endpoint{Addr: myAddr}, clientv3.WithLease(lease.ID))

	ch, _ := cli.KeepAlive(context.TODO(), lease.ID)
	go func() {
		for range ch {
		}
	}()
	fmt.Printf("✅ 服务已注册到 Etcd: %s\n", myAddr)
}

func initDB() {
	dsn := config.Conf.MySQL.DSN
	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("MySQL 连接成功！")
}

func main() {
	shutdown := tracer.InitTracer("product-service", "localhost:4318")
	defer shutdown(context.Background())
	config.InitConfig("product")
	// 统一端口键：server.port
	port := config.Conf.Server.Port
	if port == "" {
		port = "50051"
	}

	initDB()
	initRedis()    // 1. 连 Redis
	preheatStock() // 2. 预热库存
	RegisterEtcd(port)

	//新端口暴露 Prometheus
	go func() {
		//拼接冒号":9091"
		metricsAddr := fmt.Sprintf(":%s", config.Conf.Server.MetricsPort)

		//===新增开发环境重置接口===
		if config.Conf.Server.Mode == "debug" {
			fmt.Println("警告：当前为开发环境，启用重置接口 /dev/reset")

			//警告：仅限开发环境使用
			http.HandleFunc("/dev/reset", func(w http.ResponseWriter, r *http.Request) {
				//清空Redis
				err := rdb.FlushDB(context.Background()).Err()
				if err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte("清空Redis失败: " + err.Error()))
					return
				}

				if err := db.Exec("TRUNCATE TABLE `orders`").Error; err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					w.Write([]byte("MySQL 订单表重置失败: " + err.Error()))
					return
				}
				fmt.Println("MySQL 订单表已清空")

				preheatStock()

				w.Write([]byte("环境已重置，Redis已清空并重新预热库存"))
			})
		}
		http.Handle("/metrics", promhttp.Handler())
		fmt.Println("商品监控服务已启动：" + metricsAddr)
		http.ListenAndServe(metricsAddr, nil)
	}()

	grpcAddr := fmt.Sprintf(":%s", config.Conf.Server.Port)
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("监听失败：%v", err)
	}
	s := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),

		//加上prometheus监控拦截器
		grpc.StreamInterceptor(grpc_prometheus.StreamServerInterceptor),
		grpc.UnaryInterceptor(grpc_prometheus.UnaryServerInterceptor),
	)

	pb.RegisterProductServiceServer(s, &server{})

	grpc_prometheus.Register(s)

	fmt.Println("=== 商品微服务 (Redis版) 已启动 ===")

	if err := s.Serve(lis); err != nil {
		log.Fatalf("服务启动失败：%v", err)
	}
}
