package hub

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSessionManager_创建读取撤销与过期(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	randomSource := bytes.NewReader(bytes.Repeat([]byte{7}, sessionTokenBytes*2))
	manager := newSessionManager(randomSource, func() time.Time { return now })
	encodedToken, value, err := manager.create("admin", 3)
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	if value.csrfToken == "" || value.revision != 3 {
		t.Fatalf("会话内容 = %#v", value)
	}
	loaded, ok := manager.get(encodedToken)
	if !ok || loaded.username != "admin" {
		t.Fatalf("读取会话 loaded=%#v ok=%v", loaded, ok)
	}
	manager.revoke(encodedToken)
	if _, ok := manager.get(encodedToken); ok {
		t.Fatal("撤销后的会话仍有效")
	}
	if _, ok := manager.get(encodedToken + "extra"); ok {
		t.Fatal("超长会话 token 不应被接受")
	}

	randomSource = bytes.NewReader(bytes.Repeat([]byte{9}, sessionTokenBytes*2))
	manager = newSessionManager(randomSource, func() time.Time { return now })
	encodedToken, _, err = manager.create("admin", 4)
	if err != nil {
		t.Fatalf("再次创建会话失败: %v", err)
	}
	now = now.Add(sessionTTL)
	if _, ok := manager.get(encodedToken); ok {
		t.Fatal("到期会话仍有效")
	}
}

func TestLoginLimiter_第五次失败后限流且窗口恢复(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter(func() time.Time { return now })
	for attempt := 0; attempt < loginFailureLimit; attempt++ {
		reservation, _, allowed := limiter.begin("client")
		if !allowed {
			t.Fatalf("第 %d 次尝试被过早限流", attempt+1)
		}
		reservation.failure()
	}
	if _, retry, allowed := limiter.begin("client"); allowed || retry != loginWindow {
		t.Fatalf("限流结果 allowed=%v retry=%v", allowed, retry)
	}
	now = now.Add(loginWindow)
	reservation, _, allowed := limiter.begin("client")
	if !allowed {
		t.Fatal("窗口结束后仍被限流")
	}
	reservation.success()
}

func TestSessionManager_并发创建读取与撤销(t *testing.T) {
	manager := newSessionManager(rand.Reader, time.Now)
	const goroutineCount = 32
	errorsChannel := make(chan error, goroutineCount)
	var waitGroup sync.WaitGroup
	for index := 0; index < goroutineCount; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			token, _, err := manager.create(fmt.Sprintf("admin-%d", index), uint64(index+1))
			if err != nil {
				errorsChannel <- err
				return
			}
			value, ok := manager.get(token)
			if !ok || value.revision != uint64(index+1) {
				errorsChannel <- fmt.Errorf("读取并发会话失败: index=%d ok=%v revision=%d", index, ok, value.revision)
				return
			}
			manager.revoke(token)
			if _, ok := manager.get(token); ok {
				errorsChannel <- fmt.Errorf("撤销后的并发会话仍有效: index=%d", index)
			}
		}(index)
	}
	waitGroup.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Error(err)
		}
	}
}

func TestLoginLimiter_并发预占不超过失败上限(t *testing.T) {
	limiter := newLoginLimiter(time.Now)
	const goroutineCount = 32
	reservations := make(chan *loginAttempt, goroutineCount)
	var waitGroup sync.WaitGroup
	for index := 0; index < goroutineCount; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			reservation, _, allowed := limiter.begin("shared-client")
			if allowed {
				reservations <- reservation
			}
		}()
	}
	waitGroup.Wait()
	close(reservations)

	allowedCount := 0
	for reservation := range reservations {
		allowedCount++
		reservation.cancel()
	}
	if allowedCount != loginFailureLimit {
		t.Fatalf("并发允许次数=%d，期望 %d", allowedCount, loginFailureLimit)
	}
}

func TestLoginLimiter_高基数Key保持容量上限并清理过期窗口(t *testing.T) {
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	limiter := newLoginLimiter(func() time.Time { return now })
	for index := 0; index < maxLoginLimiterEntries; index++ {
		reservation, _, allowed := limiter.begin(fmt.Sprintf("client-%d", index))
		if !allowed {
			t.Fatalf("第 %d 个 key 被过早拒绝", index+1)
		}
		reservation.failure()
	}
	if _, _, allowed := limiter.begin("overflow-client"); allowed {
		t.Fatal("超过容量上限的新 key 应被拒绝")
	}
	if len(limiter.windows) != maxLoginLimiterEntries {
		t.Fatalf("限流 map 大小=%d，期望 %d", len(limiter.windows), maxLoginLimiterEntries)
	}

	now = now.Add(loginWindow)
	reservation, _, allowed := limiter.begin("recovered-client")
	if !allowed {
		t.Fatal("过期窗口清理后新 key 仍被拒绝")
	}
	reservation.cancel()
	if len(limiter.windows) != 0 {
		t.Fatalf("过期窗口清理后 map 大小=%d", len(limiter.windows))
	}
}
