package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	exportDirectoryPattern = "ark-store-export-*"
	exportFilename         = "ark.db"
	exportPageBatch        = 128
)

type onlineBackuper interface {
	NewBackup(string) (*sqlite.Backup, error)
}

type exportReadCloser struct {
	file      *os.File
	directory string
	once      sync.Once
	err       error
}

// ExportSnapshot 在线导出一份可独立恢复的状态库快照。
// @param ctx 控制在线备份、完整性校验和锁等待的取消与超时。
// @return io.ReadCloser 快照数据流；调用方必须关闭以删除 0700 临时目录和 0600 文件。
// @return error 导出、完整性校验、权限设置或清理失败时的错误。
func (s *Store) ExportSnapshot(ctx context.Context) (io.ReadCloser, error) {
	return s.exportSnapshot(ctx, "")
}

func (s *Store) exportSnapshot(ctx context.Context, temporaryRoot string) (_ io.ReadCloser, err error) {
	if ctx == nil {
		return nil, fmt.Errorf("导出状态库失败: context 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("导出状态库失败: %w", err)
	}
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("导出状态库失败: store 不能为空")
	}

	directory, err := os.MkdirTemp(temporaryRoot, exportDirectoryPattern)
	if err != nil {
		return nil, fmt.Errorf("创建状态库导出临时目录失败: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			err = errors.Join(err, removeExportDirectory(directory))
		}
	}()
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("设置状态库导出临时目录权限失败: %w", err)
	}

	path := filepath.Join(directory, exportFilename)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("创建状态库导出文件失败: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(
			fmt.Errorf("设置状态库导出文件权限失败: %w", err),
			closeExportFile(file),
		)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("关闭预创建的状态库导出文件失败: %w", err)
	}

	if err := exportOnline(ctx, s.db, path); err != nil {
		return nil, err
	}
	if err := normalizeExportedDatabase(ctx, path); err != nil {
		return nil, err
	}
	if err := validateExportedDatabase(ctx, path); err != nil {
		return nil, err
	}

	file, err = os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开状态库导出文件失败: %w", err)
	}
	cleanup = false
	return &exportReadCloser{file: file, directory: directory}, nil
}

func exportOnline(ctx context.Context, db *sql.DB, destination string) error {
	err := withConfiguredConnection(ctx, db, func(conn *sql.Conn) (operationErr error) {
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			return fmt.Errorf("开始 SQLite 导出读事务失败: %w", err)
		}
		transactionActive := true
		defer func() {
			if transactionActive {
				operationErr = errors.Join(operationErr, rollbackExportSnapshot(conn))
			}
		}()

		// WAL 读事务只有在第一次读取时才固定快照。先读 sqlite_schema，后续分批
		// backup 即使遇到持续写入也不会反复从头开始，同时不会阻塞 writer。
		var schemaObjects int
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema").Scan(&schemaObjects); err != nil {
			return fmt.Errorf("建立 SQLite 导出读快照失败: %w", err)
		}

		if err := conn.Raw(func(driverConnection any) (rawErr error) {
			backuper, ok := driverConnection.(onlineBackuper)
			if !ok {
				return fmt.Errorf("当前 SQLite 驱动不支持 online backup")
			}

			backup, err := backuper.NewBackup(dataSourceName(destination))
			if err != nil {
				return fmt.Errorf("初始化 SQLite online backup 失败: %w", err)
			}
			defer func() {
				if backup != nil {
					rawErr = errors.Join(rawErr, finishOnlineBackup(backup))
				}
			}()

			lockDeadline := time.Now().Add(time.Duration(busyTimeoutMilliseconds) * time.Millisecond)
			for {
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("SQLite online backup 已取消: %w", err)
				}

				more, err := backup.Step(exportPageBatch)
				if err == nil {
					if !more {
						break
					}
					// 每次成功推进后重新计算锁等待窗口，避免活跃写入让大库误触总超时。
					lockDeadline = time.Now().Add(time.Duration(busyTimeoutMilliseconds) * time.Millisecond)
					continue
				}
				if !isBackupLockError(err) || !time.Now().Before(lockDeadline) {
					return fmt.Errorf("执行 SQLite online backup 失败: %w", err)
				}
				if err := waitForExportRetry(ctx); err != nil {
					return fmt.Errorf("SQLite online backup 等待锁时取消: %w", err)
				}
			}

			if err := backup.Finish(); err != nil {
				backup = nil
				return fmt.Errorf("完成 SQLite online backup 失败: %w", err)
			}
			backup = nil
			return nil
		}); err != nil {
			return err
		}
		if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			return fmt.Errorf("结束 SQLite 导出读事务失败: %w", err)
		}
		transactionActive = false
		return nil
	})
	if err != nil {
		return fmt.Errorf("在线导出状态库失败: %w", err)
	}
	return nil
}

func rollbackExportSnapshot(conn *sql.Conn) error {
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		return fmt.Errorf("回滚 SQLite 导出读事务失败: %w", err)
	}
	return nil
}

func finishOnlineBackup(backup *sqlite.Backup) error {
	if err := backup.Finish(); err != nil {
		return fmt.Errorf("清理 SQLite online backup 失败: %w", err)
	}
	return nil
}

func waitForExportRetry(ctx context.Context) error {
	timer := time.NewTimer(time.Duration(busyRetryIntervalMilliseconds) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isBackupLockError(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xff
	return code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED
}

func normalizeExportedDatabase(ctx context.Context, path string) (err error) {
	db, err := sql.Open("sqlite", dataSourceName(path))
	if err != nil {
		return fmt.Errorf("打开待归一化的导出状态库失败: %w", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭导出状态库归一化连接失败: %w", closeErr))
		}
	}()

	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&journalMode); err != nil {
		return fmt.Errorf("把导出状态库归一化为单文件失败: %w", err)
	}
	if !strings.EqualFold(journalMode, "delete") {
		return fmt.Errorf("把导出状态库归一化为单文件失败: 期望 delete，实际 %q", journalMode)
	}
	return nil
}

func validateExportedDatabase(ctx context.Context, path string) (err error) {
	db, err := sql.Open("sqlite", readOnlyDataSourceName(path))
	if err != nil {
		return fmt.Errorf("打开导出的状态库失败: %w", err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭导出状态库校验连接失败: %w", closeErr))
		}
	}()

	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("校验导出状态库完整性失败: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(integrity), "ok") {
		return fmt.Errorf("校验导出状态库完整性失败: integrity_check 返回 %q", integrity)
	}

	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("校验导出状态库外键失败: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("关闭导出状态库外键校验结果失败: %w", closeErr))
		}
	}()
	if rows.Next() {
		return fmt.Errorf("校验导出状态库外键失败: foreign_key_check 返回异常记录")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取导出状态库外键校验结果失败: %w", err)
	}
	return nil
}

func readOnlyDataSourceName(path string) string {
	query := url.Values{}
	query.Set("mode", "ro")
	query.Add("_pragma", "foreign_keys(ON)")
	query.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyRetryIntervalMilliseconds))
	return (&url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(path),
		RawQuery: query.Encode(),
	}).String()
}

func (r *exportReadCloser) Read(p []byte) (int, error) {
	return r.file.Read(p)
}

func (r *exportReadCloser) Close() error {
	r.once.Do(func() {
		r.err = errors.Join(closeExportFile(r.file), removeExportDirectory(r.directory))
	})
	return r.err
}

func closeExportFile(file *os.File) error {
	if file == nil {
		return nil
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭状态库导出文件失败: %w", err)
	}
	return nil
}

func removeExportDirectory(directory string) error {
	if err := os.RemoveAll(directory); err != nil {
		return fmt.Errorf("清理状态库导出临时目录失败: %w", err)
	}
	return nil
}
