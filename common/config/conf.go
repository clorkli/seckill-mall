package config

import (
	"log"
	"os"

	"github.com/spf13/viper"
)

type Config struct {
	Server  ServerConfig  `mapstructure:"server"`
	MySQL   MySQLConfig   `mapstructure:"mysql"`
	MQ      MQConfig      `mapstructure:"mq"`
	Redis   RedisConfig   `mapstructure:"redis"`
	Etcd    EtcdConfig    `mapstructure:"etcd"`
	Seckill SeckillConfig `mapstructure:"seckill"`
	JWT     JWTConfig     `mapstructure:"jwt"`
}

type ServerConfig struct {
	Name        string `mapstructure:"name" yaml:"name"`
	Mode        string `mapstructure:"mode"`
	Port        string `mapstructure:"port" yaml:"port"`
	MetricsPort string `mapstructure:"metrics_port"`
}

type MySQLConfig struct {
	DSN string `mapstructure:"dsn"`
}

type MQConfig struct {
	URL string `mapstructure:"url"`
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type EtcdConfig struct {
	Addr string `mapstructure:"addr"`
}

type SeckillConfig struct {
	PurchaseLimit int64 `mapstructure:"purchase_limit"`
}

type JWTConfig struct {
	Expire string `mapstructure:"expire"` //对应 yaml 里的 "24h"
	Secret string `mapstructure:"secret"`
}

// 全局配置变量
var Conf *Config

// InitConfig 读取配置文件
func InitConfig(filename string) {
	viper.AddConfigPath("./config")       // 配置文件夹路径
	viper.AddConfigPath(".")              // 搜索当前根目录
	viper.AddConfigPath("./seckill-mall") // 防止在子目录下运行找不到

	viper.SetConfigName(filename) // 动态文件名
	viper.SetConfigType("yaml")   // 文件格式

	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("读取配置文件失败: %v", err)
	}

	// 将读取的配置映射到结构体中
	if err := viper.Unmarshal(&Conf); err != nil {
		log.Fatalf("解析配置文件失败: %v", err)
	}

	applyEnvOverrides()

	log.Println("配置加载成功！")
}

func applyEnvOverrides() {
	if dsn := os.Getenv("SECKILL_MYSQL_DSN"); dsn != "" {
		Conf.MySQL.DSN = dsn
	}

	if secret := os.Getenv("SECKILL_JWT_SECRET"); secret != "" {
		Conf.JWT.Secret = secret
	}

	if mqURL := os.Getenv("SECKILL_MQ_URL"); mqURL != "" {
		Conf.MQ.URL = mqURL
	}
}
