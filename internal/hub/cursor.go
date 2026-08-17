package hub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

const cursorVersion = 1

type pageCursor struct {
	Version     int    `json:"v"`
	Kind        string `json:"kind"`
	Filter      string `json:"filter"`
	StartedAtMS int64  `json:"started_at_ms"`
	ID          string `json:"id"`
}

func encodeCursor(kind string, filter string, startedAt time.Time, id string) (string, error) {
	if startedAt.IsZero() || strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("生成分页游标失败: 时间或 ID 为空")
	}
	payload, err := json.Marshal(pageCursor{
		Version: cursorVersion, Kind: kind, Filter: filter,
		StartedAtMS: startedAt.UTC().UnixMilli(), ID: id,
	})
	if err != nil {
		return "", fmt.Errorf("生成分页游标失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeCursor(value string, kind string, filter string) (time.Time, string, error) {
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("cursor 不是合法的 base64url")
	}
	var cursor pageCursor
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return time.Time{}, "", fmt.Errorf("cursor 内容无效")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return time.Time{}, "", fmt.Errorf("cursor 内容无效")
	}
	if cursor.Version != cursorVersion || cursor.Kind != kind || cursor.Filter != filter ||
		cursor.StartedAtMS < 0 || strings.TrimSpace(cursor.ID) == "" {
		return time.Time{}, "", fmt.Errorf("cursor 与当前查询不匹配")
	}
	return time.UnixMilli(cursor.StartedAtMS).UTC(), cursor.ID, nil
}
