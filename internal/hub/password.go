package hub

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	passwordMinBytes = 12
	passwordMaxBytes = 1024

	argonMemory      = 19 * 1024
	argonTime        = 2
	argonParallelism = 1
	argonSaltBytes   = 16
	argonKeyBytes    = 32

	maxArgonMemory      = 64 * 1024
	maxArgonTime        = 10
	maxArgonParallelism = 4
	maxArgonSaltBytes   = 64
	maxArgonKeyBytes    = 64
)

type passwordParameters struct {
	memory      uint32
	time        uint32
	parallelism uint8
	salt        []byte
	key         []byte
}

func validatePassword(password []byte) error {
	if len(password) < passwordMinBytes || len(password) > passwordMaxBytes {
		return fmt.Errorf("密码长度必须为 %d-%d 字节", passwordMinBytes, passwordMaxBytes)
	}
	return nil
}

func hashPassword(password []byte) (string, error) {
	if err := validatePassword(password); err != nil {
		return "", err
	}
	salt := make([]byte, argonSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成密码盐失败: %w", err)
	}
	return hashPasswordWithSalt(password, salt), nil
}

func hashPasswordWithSalt(password, salt []byte) string {
	key := argon2.IDKey(password, salt, argonTime, argonMemory, argonParallelism, argonKeyBytes)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		argonMemory,
		argonTime,
		argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

func verifyPassword(password []byte, encoded string) (bool, error) {
	parameters, err := parsePasswordHash(encoded)
	if err != nil {
		return false, err
	}
	actual := argon2.IDKey(
		password,
		parameters.salt,
		parameters.time,
		parameters.memory,
		parameters.parallelism,
		uint32(len(parameters.key)),
	)
	return subtle.ConstantTimeCompare(actual, parameters.key) == 1, nil
}

func parsePasswordHash(encoded string) (passwordParameters, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return passwordParameters{}, errors.New("密码哈希不是合法的 Argon2id PHC")
	}
	version, err := parsePrefixedUint32(parts[2], "v=")
	if err != nil || version != argon2.Version {
		return passwordParameters{}, fmt.Errorf("密码哈希版本非法")
	}

	parameterParts := strings.Split(parts[3], ",")
	if len(parameterParts) != 3 {
		return passwordParameters{}, errors.New("密码哈希参数格式非法")
	}
	memory, err := parsePrefixedUint32(parameterParts[0], "m=")
	if err != nil || memory < 8*1024 || memory > maxArgonMemory {
		return passwordParameters{}, errors.New("密码哈希 memory 参数越界")
	}
	timeCost, err := parsePrefixedUint32(parameterParts[1], "t=")
	if err != nil || timeCost == 0 || timeCost > maxArgonTime {
		return passwordParameters{}, errors.New("密码哈希 time 参数越界")
	}
	parallelismValue, err := parsePrefixedUint32(parameterParts[2], "p=")
	if err != nil || parallelismValue == 0 || parallelismValue > maxArgonParallelism {
		return passwordParameters{}, errors.New("密码哈希 parallelism 参数越界")
	}

	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) < argonSaltBytes || len(salt) > maxArgonSaltBytes {
		return passwordParameters{}, errors.New("密码哈希 salt 非法")
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(key) < argonKeyBytes/2 || len(key) > maxArgonKeyBytes {
		return passwordParameters{}, errors.New("密码哈希 key 非法")
	}
	return passwordParameters{
		memory:      memory,
		time:        timeCost,
		parallelism: uint8(parallelismValue),
		salt:        salt,
		key:         key,
	}, nil
}

func parsePrefixedUint32(value, prefix string) (uint32, error) {
	if !strings.HasPrefix(value, prefix) || len(value) == len(prefix) {
		return 0, fmt.Errorf("缺少参数前缀 %q", prefix)
	}
	parsed, err := strconv.ParseUint(value[len(prefix):], 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(parsed), nil
}

func passwordMatches(left, right []byte) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare(left, right) == 1
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
	// 密码生命周期结束后主动清零，并阻止编译器提前判定切片不再使用。
	runtime.KeepAlive(value)
}
