package main

import (
	"log"
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
	log.Printf("api gateway started addr=%s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatalf("api gateway stopped: %v", err)
	}
}
