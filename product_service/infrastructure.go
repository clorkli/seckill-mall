package main

import (
	"context"
	"fmt"
	"log"
	"strconv"

	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/naming/endpoints"

	"seckill-mall/common/config"
)

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
	fmt.Printf("服务已注册到 Etcd: %s\n", myAddr)
}

func initDB() {
	dsn := config.Conf.MySQL.DSN
	if dsn == "" {
		log.Fatal("mysql.dsn 为空，请在 config/product.yaml 设置或通过环境变量 SECKILL_MYSQL_DSN 注入")
	}

	var err error
	db, err = gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("MySQL 连接成功！")
}
