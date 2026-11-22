package store

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const userCols = `id, email, password_hash, created_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.CreatedAt)
	return u, err
}

// CreateUser 创建用户. email 应已被调用方规范化 (trim+lower); 唯一冲突
// (uq_users_email_lower) 返回 23505 — 用 IsUniqueViolation 判定映射业务错误.
func (s *Store) CreateUser(ctx context.Context, email, passwordHash string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		INSERT INTO users (email, password_hash)
		VALUES ($1, $2)
		RETURNING `+userCols, email, passwordHash))
}

// GetUserByEmail 大小写不敏感邮箱查找 (走 uq_users_email_lower 索引).
func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE lower(email) = lower($1)`, email))
	return u, mapNoRows(err)
}

// GetUserByID 主键查找.
func (s *Store) GetUserByID(ctx context.Context, id uuid.UUID) (User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE id=$1`, id))
	return u, mapNoRows(err)
}
