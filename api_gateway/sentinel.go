package main

import (
	"log"

	sentinel "github.com/alibaba/sentinel-golang/api"
	"github.com/alibaba/sentinel-golang/core/flow"
)

func initSentinel() {
	const (
		resourceName = "create_order"
		threshold    = float64(1000) //将限流阈值与日志输出绑定
	)

	// 初始化 Sentinel
	err := sentinel.InitDefault()
	if err != nil {
		log.Fatalf("sentinel init failed: %v", err)
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
		log.Fatalf("sentinel load rules failed: %v", err)
	}

	log.Printf("sentinel rule loaded resource=%s threshold_per_second=%.0f", resourceName, threshold)
}
