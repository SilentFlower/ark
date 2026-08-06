// Package config 加载并校验 ark 的备份清单（ark.yaml）。
//
// 清单是纯声明式的：它只描述「这台机器上有哪些东西需要被备份」，
// 不描述「怎么备份」——执行细节由各 target 的执行器负责。
//
// 校验刻意分成两层：
//   - Validate 只做静态语义校验（字段是否齐全、路径是否绝对、ID 是否重复），
//     全程不碰文件系统。因此中心机可以校验任意一台机器的清单，而不需要
//     那台机器上的文件真的存在。
//   - 运行环境校验（文件是否存在、权限是否安全、docker/restic 是否可用、
//     compose service 和 volume 是否真实存在）属于 doctor 包的职责。
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersion 是当前支持的清单格式版本。
// 清单格式发生不兼容变更时递增，并在 Load 中提供迁移路径。
const SchemaVersion = 1

// 各项默认值。默认备份窗口刻意避开整点和半点：
// 全世界的定时任务都挤在 0 分和 30 分，错开几分钟能显著降低
// 对象存储侧的并发冲突和限流概率。
const (
	DefaultRepoType         = "restic"
	DefaultOnCalendar       = "*-*-* 04:17:00" // 每天 04:17，即 RPO 24h
	DefaultRetentionDaily   = 7
	DefaultRetentionWeekly  = 4
	DefaultRetentionMonthly = 6
)

// hostPattern 限制 host 的取值范围。
// host 会成为对象存储 key 的一段路径，因此不允许出现需要转义的字符。
var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// TargetType 是备份目标的类型。
type TargetType string

const (
	// TargetPostgres 通过 pg_dump 做逻辑备份。
	// 绝不能改成直接打包 PGDATA 卷：运行中的数据目录热拷贝出来
	// 几乎必然是不一致的，恢复时可能根本起不来。
	TargetPostgres TargetType = "postgres"

	// TargetRedis 触发 BGSAVE 后取走 RDB 快照。
	TargetRedis TargetType = "redis"

	// TargetVolume 打包一个 docker volume 的全部内容。
	TargetVolume TargetType = "volume"

	// TargetFiles 备份宿主机上的文件（compose 文件、.env、反代配置等）。
	TargetFiles TargetType = "files"

	// TargetImageDigest 记录当前运行容器的镜像 digest。
	// 恢复时必须按 digest 拉取而不是按 tag：tag 是可变的，
	// 半年后的 :latest 与备份时的数据库 schema 很可能已经对不上。
	TargetImageDigest TargetType = "image_digest"
)

// Config 是一台机器上的完整备份清单。
type Config struct {
	Version   int       `yaml:"version"`
	Host      string    `yaml:"host"`
	Project   Project   `yaml:"project"`
	Repo      Repo      `yaml:"repo"`
	Targets   []Target  `yaml:"targets"`
	Schedule  Schedule  `yaml:"schedule"`
	Retention Retention `yaml:"retention"`

	// path 记录清单自身的来源路径，仅用于错误信息，不参与序列化。
	path string
}

// Project 定位被备份的 docker compose 项目。
type Project struct {
	// Name 是这套服务的逻辑名称，用于快照标签。
	Name string `yaml:"name"`
	// ComposeFile 是 docker-compose.yml 的绝对路径。
	ComposeFile string `yaml:"compose_file"`
	// EnvFile 是 compose 使用的 .env 绝对路径，可为空。
	EnvFile string `yaml:"env_file"`
	// ProjectName 对应 docker compose -p，为空时由 compose 按目录名推导。
	// 显式声明是为了避免误操作同一台机器上的另一套部署。
	ProjectName string `yaml:"project_name"`
}

// Repo 描述备份仓库（当前只支持 restic）。
type Repo struct {
	// Type 目前固定为 restic。保留该字段是为了后续可能的其它后端。
	Type string `yaml:"type"`
	// URL 是 restic 仓库地址，例如 s3:https://<account>.r2.cloudflarestorage.com/<bucket>/<host>。
	URL string `yaml:"url"`
	// PasswordFile 是 restic 仓库密码文件的绝对路径。
	//
	// 这个文件必须同时存在于生产机之外的地方（密码管理器、离线介质）。
	// 只存在于生产机上的仓库密码，在机器损毁时会让所有备份一起变成废数据。
	PasswordFile string `yaml:"password_file"`
	// EnvFile 存放对象存储凭证（AWS_ACCESS_KEY_ID 等）的绝对路径。
	EnvFile string `yaml:"env_file"`
}

// Target 是一个备份目标。
//
// 这里刻意用扁平结构而不是每种类型一个嵌套对象：清单是人手写的，
// 扁平结构写起来最短。代价是需要 Validate 来拒绝「字段填在了错误的类型下」，
// 见 allowedFields。
type Target struct {
	Type TargetType `yaml:"type"`

	// Service 是 compose 中的服务名（postgres / redis 类型使用）。
	Service string `yaml:"service,omitempty"`
	// Database 是要导出的数据库名（postgres 类型使用）。
	Database string `yaml:"database,omitempty"`
	// User 是执行 pg_dump 的数据库用户（postgres 类型可选）。
	User string `yaml:"user,omitempty"`

	// Name 是 volume 名称，或 files 类型的归档名。
	Name string `yaml:"name,omitempty"`

	// Paths 是要备份的宿主机路径列表（files 类型使用）。
	Paths []string `yaml:"paths,omitempty"`

	// Services 是要记录镜像 digest 的服务列表（image_digest 类型使用）。
	Services []string `yaml:"services,omitempty"`
}

// Schedule 描述备份触发时机。
//
// 用 systemd OnCalendar 而不是进程内 cron：备份 agent 是 oneshot 进程，
// 跑完就退出。没有常驻进程就没有「守护进程自己挂了导致三个月没备份」这种故障。
type Schedule struct {
	OnCalendar string `yaml:"on_calendar"`
}

// Retention 是 restic forget 的保留策略。
type Retention struct {
	Daily   int `yaml:"daily"`
	Weekly  int `yaml:"weekly"`
	Monthly int `yaml:"monthly"`
}

// Load 从磁盘读取并解析清单，随后填充默认值。
// 它不做校验，调用方通常应该直接用 LoadAndValidate。
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开清单失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	dec := yaml.NewDecoder(f)
	// 未知字段一律报错。备份清单里的拼写错误如果被静默忽略，
	// 会让人以为某个目标已经在备份、而实际上它从未被执行过——
	// 这种错误只会在真正需要恢复的那天暴露。
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("清单 %s 是空文件", path)
		}
		return nil, fmt.Errorf("解析清单 %s 失败: %w", path, err)
	}

	cfg.path = path
	cfg.applyDefaults()
	return &cfg, nil
}

// LoadAndValidate 读取清单并执行静态校验。
func LoadAndValidate(path string) (*Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Path 返回清单自身的来源路径。
func (c *Config) Path() string { return c.path }

// applyDefaults 为未填写的字段补默认值。
func (c *Config) applyDefaults() {
	if c.Version == 0 {
		c.Version = SchemaVersion
	}
	if c.Repo.Type == "" {
		c.Repo.Type = DefaultRepoType
	}
	if c.Schedule.OnCalendar == "" {
		c.Schedule.OnCalendar = DefaultOnCalendar
	}
	// 保留策略三个值同时为 0 才套用默认值。
	// 只要用户显式配了其中任何一个，就完全尊重用户的选择，
	// 避免「我明明只想留 3 天，却被悄悄补上了周备和月备」。
	if c.Retention.Daily == 0 && c.Retention.Weekly == 0 && c.Retention.Monthly == 0 {
		c.Retention.Daily = DefaultRetentionDaily
		c.Retention.Weekly = DefaultRetentionWeekly
		c.Retention.Monthly = DefaultRetentionMonthly
	}
}

// allowedFields 定义每种 target 类型允许出现哪些可选字段。
// 出现在集合之外的字段会被 Validate 拒绝，而不是静默忽略。
var allowedFields = map[TargetType]map[string]bool{
	TargetPostgres:    {"service": true, "database": true, "user": true},
	TargetRedis:       {"service": true},
	TargetVolume:      {"name": true},
	TargetFiles:       {"name": true, "paths": true},
	TargetImageDigest: {"services": true},
}

// filledFields 返回该 target 实际填写了哪些可选字段。
func (t Target) filledFields() []string {
	var out []string
	if t.Service != "" {
		out = append(out, "service")
	}
	if t.Database != "" {
		out = append(out, "database")
	}
	if t.User != "" {
		out = append(out, "user")
	}
	if t.Name != "" {
		out = append(out, "name")
	}
	if len(t.Paths) > 0 {
		out = append(out, "paths")
	}
	if len(t.Services) > 0 {
		out = append(out, "services")
	}
	return out
}

// ID 返回 target 在一次快照内的唯一标识，同时用作归档路径的一段。
func (t Target) ID() string {
	switch t.Type {
	case TargetPostgres:
		return fmt.Sprintf("postgres/%s/%s", t.Service, t.Database)
	case TargetRedis:
		return "redis/" + t.Service
	case TargetVolume:
		return "volume/" + t.Name
	case TargetFiles:
		return "files/" + t.Name
	case TargetImageDigest:
		return "image_digest"
	default:
		return string(t.Type)
	}
}

// Validate 执行静态语义校验，不访问文件系统。
// 所有问题会被一次性收集后返回，避免「改一个报一个」的往返。
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.Version != SchemaVersion {
		add("version: 期望 %d，实际 %d", SchemaVersion, c.Version)
	}

	if c.Host == "" {
		add("host: 不能为空（用于区分对象存储中不同机器的备份）")
	} else if !hostPattern.MatchString(c.Host) {
		add("host: %q 非法，只允许小写字母、数字和中划线，且不能以中划线开头或结尾", c.Host)
	}

	c.validateProject(add)
	c.validateRepo(add)
	c.validateSchedule(add)
	c.validateRetention(add)
	c.validateTargets(add)

	return errors.Join(errs...)
}

// validateProject 校验 compose 项目定位信息。
func (c *Config) validateProject(add func(string, ...any)) {
	if c.Project.Name == "" {
		add("project.name: 不能为空")
	}
	if c.Project.ComposeFile == "" {
		add("project.compose_file: 不能为空")
	} else if !filepath.IsAbs(c.Project.ComposeFile) {
		add("project.compose_file: 必须是绝对路径，实际 %q", c.Project.ComposeFile)
	}
	if c.Project.EnvFile != "" && !filepath.IsAbs(c.Project.EnvFile) {
		add("project.env_file: 必须是绝对路径，实际 %q", c.Project.EnvFile)
	}
}

// validateRepo 校验备份仓库配置。
func (c *Config) validateRepo(add func(string, ...any)) {
	if c.Repo.Type != DefaultRepoType {
		add("repo.type: 当前只支持 %q，实际 %q", DefaultRepoType, c.Repo.Type)
	}
	if c.Repo.URL == "" {
		add("repo.url: 不能为空")
	}
	if c.Repo.PasswordFile == "" {
		add("repo.password_file: 不能为空")
	} else if !filepath.IsAbs(c.Repo.PasswordFile) {
		add("repo.password_file: 必须是绝对路径，实际 %q", c.Repo.PasswordFile)
	}
	if c.Repo.EnvFile != "" && !filepath.IsAbs(c.Repo.EnvFile) {
		add("repo.env_file: 必须是绝对路径，实际 %q", c.Repo.EnvFile)
	}
}

// validateSchedule 只检查非空。
// OnCalendar 的语法校验交给 doctor 调用 systemd-analyze 完成——
// 那里有 systemd 本身作为权威，比自己写一个近似的解析器可靠。
func (c *Config) validateSchedule(add func(string, ...any)) {
	if strings.TrimSpace(c.Schedule.OnCalendar) == "" {
		add("schedule.on_calendar: 不能为空")
	}
}

// validateRetention 拒绝负数和「全为 0」。
func (c *Config) validateRetention(add func(string, ...any)) {
	if c.Retention.Daily < 0 || c.Retention.Weekly < 0 || c.Retention.Monthly < 0 {
		add("retention: 保留份数不能为负")
	}
	if c.Retention.Daily == 0 && c.Retention.Weekly == 0 && c.Retention.Monthly == 0 {
		add("retention: 三项不能同时为 0，否则清理时会删光所有快照")
	}
}

// validateTargets 校验每个备份目标，并检查 ID 唯一性。
func (c *Config) validateTargets(add func(string, ...any)) {
	if len(c.Targets) == 0 {
		add("targets: 至少需要一个备份目标")
		return
	}

	seen := make(map[string]int, len(c.Targets))
	for i, t := range c.Targets {
		prefix := fmt.Sprintf("targets[%d]", i)

		allowed, ok := allowedFields[t.Type]
		if !ok {
			add("%s.type: 未知类型 %q", prefix, t.Type)
			continue
		}

		// 拒绝填在错误类型下的字段。静默忽略等于让用户以为配置生效了。
		for _, f := range t.filledFields() {
			if !allowed[f] {
				add("%s: 字段 %q 不适用于类型 %q", prefix, f, t.Type)
			}
		}

		validateTargetRequired(prefix, t, add)

		id := t.ID()
		if prev, dup := seen[id]; dup {
			add("%s: 与 targets[%d] 重复（同一个 %q）", prefix, prev, id)
			continue
		}
		seen[id] = i
	}
}

// validateTargetRequired 按类型检查必填字段。
func validateTargetRequired(prefix string, t Target, add func(string, ...any)) {
	switch t.Type {
	case TargetPostgres:
		if t.Service == "" {
			add("%s.service: postgres 类型必填", prefix)
		}
		if t.Database == "" {
			add("%s.database: postgres 类型必填", prefix)
		}
	case TargetRedis:
		if t.Service == "" {
			add("%s.service: redis 类型必填", prefix)
		}
	case TargetVolume:
		if t.Name == "" {
			add("%s.name: volume 类型必填", prefix)
		}
	case TargetFiles:
		if t.Name == "" {
			add("%s.name: files 类型必填（用作归档名）", prefix)
		}
		if len(t.Paths) == 0 {
			add("%s.paths: files 类型至少需要一个路径", prefix)
		}
		for j, p := range t.Paths {
			if !filepath.IsAbs(p) {
				add("%s.paths[%d]: 必须是绝对路径，实际 %q", prefix, j, p)
			}
		}
	case TargetImageDigest:
		if len(t.Services) == 0 {
			add("%s.services: image_digest 类型至少需要一个服务", prefix)
		}
	}
}
