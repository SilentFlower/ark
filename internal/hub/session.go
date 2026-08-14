package hub

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	sessionTokenBytes = 32
	// 32-byte token 的 base64url 无填充编码固定为 43 字符，先限长再解码可避免异常 Cookie 分配。
	encodedSessionTokenLength = 43
	sessionTTL                = 12 * time.Hour
)

type session struct {
	username  string
	revision  uint64
	csrfToken string
	createdAt time.Time
	expiresAt time.Time
}

type sessionManager struct {
	mutex    sync.Mutex
	sessions map[[sha256.Size]byte]session
	random   io.Reader
	now      func() time.Time
}

func newSessionManager(random io.Reader, now func() time.Time) *sessionManager {
	return &sessionManager{
		sessions: make(map[[sha256.Size]byte]session),
		random:   random,
		now:      now,
	}
}

func (manager *sessionManager) create(username string, revision uint64) (string, session, error) {
	now := manager.now().UTC()
	csrfToken, err := randomToken(manager.random)
	if err != nil {
		return "", session{}, fmt.Errorf("生成会话 CSRF token 失败: %w", err)
	}
	value := session{
		username:  username,
		revision:  revision,
		csrfToken: csrfToken,
		createdAt: now,
		expiresAt: now.Add(sessionTTL),
	}

	for attempts := 0; attempts < 3; attempts++ {
		rawToken, err := randomBytes(manager.random, sessionTokenBytes)
		if err != nil {
			return "", session{}, fmt.Errorf("生成会话 token 失败: %w", err)
		}
		key := sha256.Sum256(rawToken)
		manager.mutex.Lock()
		manager.removeExpiredLocked(now)
		_, exists := manager.sessions[key]
		if !exists {
			manager.sessions[key] = value
		}
		manager.mutex.Unlock()
		if !exists {
			return base64.RawURLEncoding.EncodeToString(rawToken), value, nil
		}
	}
	return "", session{}, fmt.Errorf("生成唯一会话 token 失败")
}

func (manager *sessionManager) get(encodedToken string) (session, bool) {
	key, ok := sessionKey(encodedToken)
	if !ok {
		return session{}, false
	}
	now := manager.now().UTC()
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	manager.removeExpiredLocked(now)
	value, ok := manager.sessions[key]
	return value, ok
}

func (manager *sessionManager) revoke(encodedToken string) {
	key, ok := sessionKey(encodedToken)
	if !ok {
		return
	}
	manager.mutex.Lock()
	delete(manager.sessions, key)
	manager.mutex.Unlock()
}

func (manager *sessionManager) removeExpiredLocked(now time.Time) {
	for key, value := range manager.sessions {
		if !now.Before(value.expiresAt) {
			delete(manager.sessions, key)
		}
	}
}

func sessionKey(encodedToken string) ([sha256.Size]byte, bool) {
	if len(encodedToken) != encodedSessionTokenLength {
		return [sha256.Size]byte{}, false
	}
	rawToken, err := base64.RawURLEncoding.Strict().DecodeString(encodedToken)
	if err != nil || len(rawToken) != sessionTokenBytes {
		return [sha256.Size]byte{}, false
	}
	return sha256.Sum256(rawToken), true
}

func randomToken(reader io.Reader) (string, error) {
	value, err := randomBytes(reader, sessionTokenBytes)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func randomBytes(reader io.Reader, size int) ([]byte, error) {
	value := make([]byte, size)
	if _, err := io.ReadFull(reader, value); err != nil {
		return nil, err
	}
	return value, nil
}
