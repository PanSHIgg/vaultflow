// migrate.go — Wave-2 I1 启动期迁移执行器.
//
//   - schema_migrations(version INT PRIMARY KEY) 跟踪已应用版本;
//   - 依序应用 migrations/001..00N 的 *.sql, 已存在版本跳过 (幂等重入);
//   - SET lock_timeout='15s' 防止 NF-3 应用层锁周期无限悬挂.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

var migrationFileRe = regexp.MustCompile(`^(\d{3})_.*\.sql$`)

// runMigrations 应用目录下全部未执行迁移; 返回已应用版本列表.
func runMigrations(ctx context.Context, pool *pgxpool.Pool, dir string, log *zap.Logger) ([]int, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: acquire conn: %w", err)
	}
	defer conn.Release()

	// NF-3: 迁移会话级 lock_timeout — DDL/锁获取超过 15s 自动放弃,
	// 避免 schema 变更在并发窗口死锁悬挂.
	if _, err := conn.Exec(ctx, `SET lock_timeout = '15s'`); err != nil {
		return nil, fmt.Errorf("migrate: set lock_timeout: %w", err)
	}
	if _, err := conn.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version INT PRIMARY KEY)`); err != nil {
		return nil, fmt.Errorf("migrate: create schema_migrations: %w", err)
	}

	applied := map[int]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, fmt.Errorf("migrate: scan version: %w", err)
		}
		applied[v] = true
	}
	rows.Close()

	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read dir %s: %w", dir, err)
	}
	type mig struct {
		ver  int
		path string
	}
	var migs []mig
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		m := migrationFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		migs = append(migs, mig{ver: v, path: filepath.Join(dir, e.Name())})
	}
	sort.Slice(migs, func(i, j int) bool { return migs[i].ver < migs[j].ver })

	var done []int
	for _, m := range migs {
		if applied[m.ver] {
			log.Debug("migration already applied, skipping", zap.Int("version", m.ver))
			continue
		}
		sql, err := os.ReadFile(m.path)
		if err != nil {
			return done, fmt.Errorf("migrate: read %s: %w", m.path, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return done, fmt.Errorf("migrate: begin v%03d: %w", m.ver, err)
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return done, fmt.Errorf("migrate: apply %s: %w", m.path, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, m.ver); err != nil {
			_ = tx.Rollback(ctx)
			return done, fmt.Errorf("migrate: record v%03d: %w", m.ver, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return done, fmt.Errorf("migrate: commit v%03d: %w", m.ver, err)
		}
		done = append(done, m.ver)
		log.Info("migration applied", zap.Int("version", m.ver), zap.String("file", filepath.Base(m.path)))
	}
	return done, nil
}
