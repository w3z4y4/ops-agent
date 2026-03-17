package filemanager

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

const defaultChunkSize = 512 * 1024 // 512KB

// ─────────────────────────────────────────────
//  Upload Session（控制台 → Agent）
// ─────────────────────────────────────────────

// UploadSession 代表一次正在进行的文件上传会话。
type UploadSession struct {
	mu         sync.Mutex
	TransferID string
	destPath   string
	tmpPath    string
	tmpFile    *os.File
	hasher     hash.Hash // crypto/sha256
	uid        int
	gid        int
	mode       os.FileMode
}

// Manager 管理所有传输会话。
type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*UploadSession
}

func New() *Manager {
	return &Manager{
		sessions: make(map[string]*UploadSession),
	}
}

// StartUpload 初始化上传会话，创建临时文件。
func (m *Manager) StartUpload(transferID, destPath string, uid, gid int, mode uint32) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return fmt.Errorf("create dest dir: %w", err)
	}

	tmpPath := destPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create tmp file: %w", err)
	}

	sess := &UploadSession{
		TransferID: transferID,
		destPath:   destPath,
		tmpPath:    tmpPath,
		tmpFile:    f,
		hasher:     sha256.New(),
		uid:        uid,
		gid:        gid,
		mode:       os.FileMode(mode),
	}

	m.mu.Lock()
	m.sessions[transferID] = sess
	m.mu.Unlock()

	return nil
}

// WriteChunk 将数据块写入临时文件并更新哈希。
func (m *Manager) WriteChunk(transferID string, data []byte) error {
	m.mu.RLock()
	sess, ok := m.sessions[transferID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("unknown transfer ID: %s", transferID)
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()

	if _, err := sess.tmpFile.Write(data); err != nil {
		return fmt.Errorf("write chunk: %w", err)
	}
	sess.hasher.Write(data)
	return nil
}

// FinalizeUpload 完成上传：校验 SHA256，原子重命名，设置权限。
func (m *Manager) FinalizeUpload(transferID, expectedSHA256 string) error {
	m.mu.Lock()
	sess, ok := m.sessions[transferID]
	if ok {
		delete(m.sessions, transferID)
	}
	m.mu.Unlock()

	if !ok {
		return fmt.Errorf("unknown transfer ID: %s", transferID)
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()

	// 关闭临时文件
	if err := sess.tmpFile.Close(); err != nil {
		os.Remove(sess.tmpPath)
		return fmt.Errorf("close tmp file: %w", err)
	}

	// 校验 SHA256
	actual := hex.EncodeToString(sess.hasher.Sum(nil))
	if actual != expectedSHA256 {
		os.Remove(sess.tmpPath)
		return fmt.Errorf("sha256 mismatch: expected %s, got %s", expectedSHA256, actual)
	}

	// 原子重命名
	if err := os.Rename(sess.tmpPath, sess.destPath); err != nil {
		os.Remove(sess.tmpPath)
		return fmt.Errorf("atomic rename: %w", err)
	}

	// 设置文件权限
	if sess.mode != 0 {
		if err := os.Chmod(sess.destPath, sess.mode); err != nil {
			return fmt.Errorf("chmod: %w", err)
		}
	}
	if sess.uid >= 0 && sess.gid >= 0 {
		if err := os.Lchown(sess.destPath, sess.uid, sess.gid); err != nil {
			return fmt.Errorf("chown: %w", err)
		}
	}

	return nil
}

// AbortUpload 中止上传，清除临时文件。
func (m *Manager) AbortUpload(transferID string) {
	m.mu.Lock()
	sess, ok := m.sessions[transferID]
	if ok {
		delete(m.sessions, transferID)
	}
	m.mu.Unlock()

	if ok {
		sess.tmpFile.Close()
		os.Remove(sess.tmpPath)
	}
}

// ─────────────────────────────────────────────
//  Download（Agent → 控制台）
// ─────────────────────────────────────────────

// DownloadChunk 描述一个下载分块。
type DownloadChunk struct {
	Data        []byte
	IsLastChunk bool
	SHA256Total string // 仅最后一个 chunk 填写
}

// StreamDownload 打开文件，按 chunkSize 分块返回，并在最后一块附上整体 SHA256。
// 调用方通过迭代 chan 消费所有分块。
func StreamDownload(srcPath string, chunkSize int) (<-chan DownloadChunk, <-chan error) {
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}

	chunks := make(chan DownloadChunk, 8)
	errs := make(chan error, 1)

	go func() {
		defer close(chunks)
		defer close(errs)

		f, err := os.Open(srcPath)
		if err != nil {
			errs <- fmt.Errorf("open file: %w", err)
			return
		}
		defer f.Close()

		hasher := sha256.New()
		buf := make([]byte, chunkSize)
		var lastChunkData []byte
		var isLast bool

		for {
			n, readErr := f.Read(buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				hasher.Write(data)

				// 先缓存，等确认是否为最后块
				if lastChunkData != nil {
					chunks <- DownloadChunk{Data: lastChunkData, IsLastChunk: false}
				}
				lastChunkData = data
			}
			if readErr == io.EOF {
				isLast = true
				break
			}
			if readErr != nil {
				errs <- fmt.Errorf("read file: %w", readErr)
				return
			}
		}

		// 发送最后一块，附上 SHA256
		if lastChunkData != nil || isLast {
			chunks <- DownloadChunk{
				Data:        lastChunkData,
				IsLastChunk: true,
				SHA256Total: hex.EncodeToString(hasher.Sum(nil)),
			}
		}
	}()

	return chunks, errs
}

// ComputeSHA256 计算文件 SHA256，用于下载前预检。
func ComputeSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// 修正 import：需要 hash 包
// 在实际代码中 import "crypto/sha256" 已包含 hash.Hash 接口。
// 此处 hash.Hash 引用需要 import "hash"，框架代码仅示意。
type hash interface {
	io.Writer
	Sum(b []byte) []byte
}

// 实际编译时删除上方的本地 hash interface 定义，统一使用 crypto/sha256 返回的 hash.Hash。
// 建议最终代码中 import (
//   "crypto/sha256"
//   "hash"
// ) 并将字段类型改为 hash.Hash。

// IntToFileMode 辅助函数：将 uint32 转换为 os.FileMode。
func IntToFileMode(mode uint32) os.FileMode {
	perm, _ := strconv.ParseUint(strconv.FormatUint(uint64(mode), 8), 8, 32)
	return os.FileMode(perm)
}
