package hub

import (
	"testing"
	"time"

	"github.com/silentflower/ark/internal/store"
)

// hostRun 构造一条运行记录。targetBytes 里每个元素是一个 target 的字节数，
// 因为一次 run 的体积是它全部 target 之和。
func hostRun(id string, status store.Status, finishedAt time.Time, targetBytes ...int64) store.HostRun {
	targets := make([]store.RunTarget, 0, len(targetBytes))
	for index, bytes := range targetBytes {
		targets = append(targets, store.RunTarget{
			RunID: id, Host: "web-01", TargetID: string(rune('a' + index)),
			TargetType: "files", Status: status, Bytes: bytes,
		})
	}
	return store.HostRun{
		Run:     store.Run{ID: id, Status: status, FinishedAt: finishedAt},
		Status:  status,
		Targets: targets,
	}
}

// TestDeriveBackupSizes 覆盖大小趋势的全部边界。
//
// 这段逻辑出错不会有人立刻发现：趋势图照样能画出来，只是画的是错的东西——
// 把失败 run 的半截字节混进去，恰恰会掩盖 ADR-011 要盯的体积腰斩信号。
func TestDeriveBackupSizes(t *testing.T) {
	base := time.Date(2026, 8, 17, 4, 17, 0, 0, time.UTC)
	at := func(dayOffset int) time.Time { return base.AddDate(0, 0, dayOffset) }

	tests := []struct {
		name       string
		runs       []store.HostRun
		wantBytes  []int64
		wantLatest *int64
	}{
		{
			name:       "没有任何运行记录",
			runs:       nil,
			wantBytes:  []int64{},
			wantLatest: nil,
		},
		{
			name:       "只有失败记录时不产生采样点",
			runs:       []store.HostRun{hostRun("r1", store.StatusFail, at(0), 100)},
			wantBytes:  []int64{},
			wantLatest: nil,
		},
		{
			name: "多个 target 的字节求和",
			runs: []store.HostRun{hostRun("r1", store.StatusOK, at(0), 100, 250, 30)},
			// 380 = 100 + 250 + 30
			wantBytes:  []int64{380},
			wantLatest: pointerTo(int64(380)),
		},
		{
			name: "warn 也计入：它是完成的备份，体积可比",
			runs: []store.HostRun{
				hostRun("r2", store.StatusWarn, at(1), 90),
				hostRun("r1", store.StatusOK, at(0), 100),
			},
			wantBytes:  []int64{100, 90},
			wantLatest: pointerTo(int64(90)),
		},
		{
			name: "失败记录被跳过，不打断成功序列",
			runs: []store.HostRun{
				hostRun("r3", store.StatusOK, at(2), 120),
				hostRun("r2", store.StatusFail, at(1), 5),
				hostRun("r1", store.StatusOK, at(0), 100),
			},
			wantBytes:  []int64{100, 120},
			wantLatest: pointerTo(int64(120)),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			points, latest := deriveBackupSizes(tc.runs)
			if len(points) != len(tc.wantBytes) {
				t.Fatalf("采样点数量 = %d，期望 %d", len(points), len(tc.wantBytes))
			}
			for index, want := range tc.wantBytes {
				if points[index].Bytes != want {
					t.Errorf("第 %d 个采样点 = %d，期望 %d", index, points[index].Bytes, want)
				}
			}
			switch {
			case tc.wantLatest == nil && latest != nil:
				t.Errorf("最近体积 = %d，期望 nil", *latest)
			case tc.wantLatest != nil && latest == nil:
				t.Errorf("最近体积 = nil，期望 %d", *tc.wantLatest)
			case tc.wantLatest != nil && *latest != *tc.wantLatest:
				t.Errorf("最近体积 = %d，期望 %d", *latest, *tc.wantLatest)
			}
		})
	}
}

// TestDeriveBackupSizes_按时间正序且有上限 单独验证顺序与截断。
// 顺序错了折线会左右翻转，看上去仍然「正常」，因此必须显式断言。
func TestDeriveBackupSizes_按时间正序且有上限(t *testing.T) {
	base := time.Date(2026, 8, 17, 4, 17, 0, 0, time.UTC)
	// 构造 20 条成功记录，store 返回的是倒序（最新在前）。
	runs := make([]store.HostRun, 0, 20)
	for index := 19; index >= 0; index-- {
		runs = append(runs, hostRun(
			string(rune('A'+index)), store.StatusOK, base.AddDate(0, 0, index), int64(index+1),
		))
	}

	points, latest := deriveBackupSizes(runs)

	if len(points) != maximumSizePoints {
		t.Fatalf("采样点数量 = %d，期望上限 %d", len(points), maximumSizePoints)
	}
	// 保留的是最近 14 次（第 6..19 天），并按时间正序排列。
	if points[0].Bytes != 7 || points[len(points)-1].Bytes != 20 {
		t.Fatalf("采样区间 = [%d, %d]，期望 [7, 20]", points[0].Bytes, points[len(points)-1].Bytes)
	}
	for index := 1; index < len(points); index++ {
		if points[index-1].FinishedAt >= points[index].FinishedAt {
			t.Fatalf("采样点未按时间正序: %q 在 %q 之前",
				points[index-1].FinishedAt, points[index].FinishedAt)
		}
	}
	if latest == nil || *latest != 20 {
		t.Fatalf("最近体积 = %v，期望 20", latest)
	}
}

func pointerTo[T any](value T) *T {
	return &value
}
