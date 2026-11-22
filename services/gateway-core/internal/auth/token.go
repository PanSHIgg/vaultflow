package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// DefaultTokenTTL 默认用户令牌有效期 24h (IssueUserToken ttl<=0 时生效).
const DefaultTokenTTL = 24 * time.Hour

// userRole 用户令牌固定角色声明.
const userRole = "user"

// ErrInvalidToken 签名/有效期/角色校验失败.
var ErrInvalidToken = errors.New("auth: invalid token")

// userClaims JWT 载荷: {sub: user_id, role: "user", iat, exp} (HS256).
type userClaims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// IssueUserToken 签发 HS256 用户令牌. ttl<=0 时取 DefaultTokenTTL.
func IssueUserToken(secret []byte, userID uuid.UUID, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	now := time.Now()
	claims := userClaims{
		Role: userRole,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// ParseUserToken 验签 + 有效期 + role="user" 校验, 返回 sub 解析的 userID.
// 任何失败统一映射 ErrInvalidToken (errors.Is 可判).
func ParseUserToken(secret []byte, tokenStr string) (uuid.UUID, error) {
	var claims userClaims
	_, err := jwt.ParseWithClaims(tokenStr, &claims,
		func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("%w: unexpected alg %s", ErrInvalidToken, t.Method.Alg())
			}
			return secret, nil
		},
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if claims.Role != userRole {
		return uuid.Nil, fmt.Errorf("%w: role=%q", ErrInvalidToken, claims.Role)
	}
	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: bad sub", ErrInvalidToken)
	}
	return userID, nil
}
