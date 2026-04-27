package main

import (
	"fmt"
	"net"

	"github.com/gin-gonic/gin"

	"seckill-mall/common/config"
)

func startHTTPServer(r *gin.Engine) {
	port := config.Conf.Server.Port
	if port == "" {
		port = "8080"
	}

	addr := net.JoinHostPort("", port)
	fmt.Printf("=== API 网关已启动 (Port: %s) ===\n", addr)
	r.Run(addr)
}
