package main

import (
	"context"
	"log"

	"seckill-mall/common/pb"
)

type server struct {
	pb.UnimplementedProductServiceServer
}

// GetProduct 实现
func (s *server) GetProduct(ctx context.Context, req *pb.ProductRequest) (*pb.ProductResponse, error) {
	log.Printf("product get requested product_id=%d", req.ProductId)

	var product Product
	if err := db.First(&product, req.ProductId).Error; err != nil {
		return nil, err
	}
	return &pb.ProductResponse{
		ProductId: product.ID, Name: product.Name, Price: product.Price,
	}, nil
}
