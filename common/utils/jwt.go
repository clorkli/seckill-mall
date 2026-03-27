package utils

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"seckill-mall/common/config"

	"github.com/golang-jwt/jwt/v5"
)

// 自定义Claims结构体
type UserClaims struct {
	UserID int64 `json:"user_id"`
	jwt.RegisteredClaims
}

func getJWTSecret() ([]byte, error) {
	if config.Conf == nil {
		return nil, errors.New("config is not initialized")
	}

	secret := strings.TrimSpace(config.Conf.JWT.Secret)
	if secret == "" {
		return nil, fmt.Errorf("jwt.secret 为空，请在配置文件中设置，或通过环境变量 SECKILL_JWT_SECRET 注入")
	}

	return []byte(secret), nil
}

// 生成Token
func GenerateToken(userID int64, expireDuration time.Duration) (string, error) {
	secret, err := getJWTSecret()
	if err != nil {
		return "", err
	}

	now := time.Now()
	expireTime := now.Add(expireDuration)

	claims := UserClaims{
		UserID: userID,
		RegisteredClaims: jwt.RegisteredClaims{
			//NotBefore: jwt.NewNumericDate(now), // 可选：生效时间
			//IssuedAt:  jwt.NewNumericDate(now), // 可选：签发时间
			ExpiresAt: jwt.NewNumericDate(expireTime),
			Issuer:    "seckill-app",
		},
	}

	tokenClaims := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tokenClaims.SignedString(secret)
}

// 解析Token
func ParseToken(token string) (*UserClaims, error) {
	secret, err := getJWTSecret()
	if err != nil {
		return nil, err
	}

	//解析并验证签名
	tokenClaims, err := jwt.ParseWithClaims(token, &UserClaims{}, func(token *jwt.Token) (interface{}, error) {
		return secret, nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := tokenClaims.Claims.(*UserClaims); ok && tokenClaims.Valid {
		return claims, nil
	}

	return nil, errors.New("invalid token")
}
