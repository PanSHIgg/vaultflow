// Package auth — 交易所托管模型的用户认证原语: bcrypt 口令散列与 HS256
// 用户 JWT. 无 refresh token (MVP); TTL 默认 24h.
package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidCredentials 口令校验失败 (mismatch 或 hash 损坏, 对外不区分).
var ErrInvalidCredentials = errors.New("auth: invalid credentials")

// HashPassword bcrypt(password), cost = bcrypt.DefaultCost.
func HashPassword(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// CheckPassword 校验明文口令与 bcrypt hash; 匹配返回 nil, 否则 ErrInvalidCredentials.
func CheckPassword(hash, pw string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)); err != nil {
		return ErrInvalidCredentials
	}
	return nil
}
