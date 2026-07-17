package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

// IsGitMetadataPath 判断 workspace 相对路径是否落在任意 .git 目录内。
// 路径分段采用大小写不敏感比较，避免在大小写不敏感文件系统（如 macOS APFS 默认配置）
// 上通过 ".GIT" 之类的写法绕过保护。
func IsGitMetadataPath(relativePath string) bool {
	relativePath = filepath.Clean(relativePath)
	if relativePath == "." {
		return false
	}
	for _, segment := range strings.Split(relativePath, string(os.PathSeparator)) {
		if strings.EqualFold(segment, ".git") {
			return true
		}
	}
	return false
}

// CanonicalAbsolute 返回绝对化并尽力解析符号链接后的路径。
func CanonicalAbsolute(path string) (string, error) {
	return canonicalAbsolute(path)
}

// CanonicalizeAllowMissing 解析路径中已存在部分的符号链接；路径不存在的尾部保持原样。
func CanonicalizeAllowMissing(path string) string {
	return canonicalizePathAllowMissing(path)
}
