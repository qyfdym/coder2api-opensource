package qoder

import (
	"os"
	"path/filepath"
)

// acctWriteFile 写测试文件（自动建父目录）。
func acctWriteFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

// acctFileExists 报告文件是否存在。
func acctFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
