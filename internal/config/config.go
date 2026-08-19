// Package config 加载并校验 ark 的备份清单（ark.yaml）。
//
// 清单是纯声明式的：它描述「hub 要管哪些机器、每台机器上有哪些东西需要备份」，
// 不描述「怎么备份」——执行细节由各 target 的执行器负责。
//
// 清单只存在于 hub 上，一份描述全部机器（ADR-002）。被备份的机器上
// 不安装任何 ark 组件，因此也不会有属于它自己的那份清单。
//
// 校验刻意分成两层：
//   - Validate 只做静态语义校验（字段是否齐全、路径是否绝对、ID 是否重复），
//     全程不碰文件系统。因此 hub 可以校验任意一台机器的清单段落，
//     而不需要真的连上那台机器。
//   - 运行环境校验（文件是否存在、权限是否安全、docker 是否可用、
//     compose service 和 volume 是否真实存在）属于 doctor 包的职责。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/silentflower/ark/internal/endpoint"
	"gopkg.in/yaml.v3"
)

// SchemaVersion 是当前支持的清单格式版本。
//
// v1 是「每台机器装一个 agent、各读各的清单」时代的单机格式。
// 架构改为 hub 集中编排后（ADR-002），顶层从「一台机器」变成「一组机器」，
// 这是一次不兼容变更，因此版本号从 1 递增到 2。
const SchemaVersion = 2

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
// host 会成为 restic 快照的 tag，也是恢复时的检索键，
// 因此不允许出现需要转义的字符。
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

// SSHHostKeyPolicy 是 OpenSSH 主机密钥校验策略。
type SSHHostKeyPolicy string

const (
	// SSHHostKeyPolicyAcceptNew 允许首次连接自动记录主机密钥，但拒绝已记录主机的密钥变化。
	SSHHostKeyPolicyAcceptNew SSHHostKeyPolicy = "accept-new"
	// SSHHostKeyPolicyStrict 要求主机密钥已经存在且完全匹配。
	SSHHostKeyPolicyStrict SSHHostKeyPolicy = "strict"
	// DefaultSSHHostKeyPolicy 是清单未显式声明时使用的主机密钥策略。
	DefaultSSHHostKeyPolicy = SSHHostKeyPolicyAcceptNew
)

// Config 是 hub 的完整备份清单，描述它管理的全部机器。
type Config struct {
	Version    int         `yaml:"version"`
	Repo       Repo        `yaml:"repo"`
	Defaults   Defaults    `yaml:"defaults"`
	Monitoring *Monitoring `yaml:"monitoring,omitempty"`
	DNSMgr     *DNSMgr     `yaml:"dnsmgr,omitempty"`
	Hosts      []Host      `yaml:"hosts"`

	// path 记录清单自身的来源路径，仅用于错误信息，不参与序列化。
	path string
}

// Defaults 是各 host 未显式覆盖时套用的默认值。
//
// 两个字段都是指针：需要区分「这段压根没写」和「写了但值为零」。
// 后者是用户的明确选择，不能被默认值覆盖。
type Defaults struct {
	Schedule  *Schedule  `yaml:"schedule"`
	Retention *Retention `yaml:"retention"`
}

// Monitoring 描述告警与外部心跳使用的受限秘密文件。
//
// 清单只保存绝对路径，Webhook、签名密钥与心跳 URL 都留在权限受限的文件中，
// 避免它们进入示例、状态库或清单备份。
type Monitoring struct {
	EnvFile string `yaml:"env_file"`
}

// DNSMgr 描述 ark 调用 dnsmgr AuthApi 所需的非秘密连接配置。
//
// UID 与 API key 只存在 EnvFile 指向的受限文件中，避免进入清单、
// dry-run、状态库或普通错误上下文。
type DNSMgr struct {
	// BaseURL 是 dnsmgr 服务的绝对 URL，不包含 API 路由。
	BaseURL string `yaml:"base_url"`
	// EnvFile 是 dnsmgr AuthApi 凭证文件的绝对路径。
	EnvFile string `yaml:"env_file"`
}

// Host 是一台被备份的机器。
type Host struct {
	// Host 是这台机器的标识，全局唯一。
	// 它会成为 restic 快照的 tag，也是恢复时用来检索的键。
	Host string `yaml:"host"`

	// Local 为 true 表示这台就是 hub 自己，命令直接在本地执行，不走 SSH。
	// hub 自身也需要被备份（ADR-012），所以它同样出现在 hosts 列表里。
	Local bool `yaml:"local"`

	// SSH 是连到这台机器的方式。Local 为 true 时必须为空。
	//
	// 用指针而不是值：判定「local 与 ssh 互斥」需要区分
	// 「写了一个空的 ssh 段」和「压根没写 ssh」，值类型做不到。
	SSH *SSH `yaml:"ssh"`

	Project Project  `yaml:"project"`
	Targets []Target `yaml:"targets"`

	// Schedule 与 Retention 为 nil 时套用 Defaults，见 ScheduleFor / RetentionFor。
	Schedule  *Schedule  `yaml:"schedule"`
	Retention *Retention `yaml:"retention"`

	// DNSMgr 描述恢复到该 host 时的 dmonitor 维护任务和 DNS 切换，可为空。
	DNSMgr *HostDNSMgr `yaml:"dnsmgr,omitempty"`
}

// HostDNSMgr 描述恢复到一台 host 时的 dmonitor 任务与 DNS 记录关联。
type HostDNSMgr struct {
	// TaskIDs 按声明顺序保存恢复窗口内需要暂停的 dmonitor 任务。
	TaskIDs []int64 `yaml:"task_ids,omitempty"`
	// Value 是所有关联 A 或 AAAA 记录要切换到的显式 IP。
	Value string `yaml:"value,omitempty"`
	// Records 按声明顺序保存需要切换的 dnsmgr 记录。
	Records []DNSMgrRecord `yaml:"records,omitempty"`
}

// DNSMgrRecord 是一条 dnsmgr 域名与 provider 记录的稳定关联。
type DNSMgrRecord struct {
	// DomainID 是 dnsmgr 本地 domain 表的主键。
	DomainID int64 `yaml:"domain_id"`
	// RecordID 是 DNS provider 返回的稳定记录 ID。
	RecordID string `yaml:"record_id"`
}

// SSH 描述 hub 连到一台目标机所需的全部信息。
//
// 目标机上不需要装任何东西，有 sshd 就够了（ADR-002）。
type SSH struct {
	// Address 是 host:port，端口必须显式写出。
	// 清单是人写一次、机器读三年的东西，显式端口既避免歧义，
	// 也和 known_hosts 里的记录形态对得上。
	Address string `yaml:"address"`
	// User 是登录用户。
	User string `yaml:"user"`
	// IdentityFile 是 SSH 私钥的绝对路径。
	//
	// 私钥本身不进备份：把开锁的钥匙和锁着的箱子放在一起没有意义。
	IdentityFile string `yaml:"identity_file"`
	// KnownHostsFile 是主机密钥库的绝对路径，必填。
	//
	// 不提供任何「跳过主机密钥校验」的开关：hub 会把生产数据流经这条连接，
	// 中间人劫持同时意味着数据泄露和恢复投毒。
	KnownHostsFile string `yaml:"known_hosts_file"`
	// HostKeyPolicy 控制首次连接是否允许 OpenSSH 自动记录主机密钥。
	// 空值使用 DefaultSSHHostKeyPolicy；不提供完全关闭校验的取值。
	HostKeyPolicy SSHHostKeyPolicy `yaml:"host_key_policy,omitempty"`
}

// EffectiveHostKeyPolicy 返回 SSH 配置实际生效的主机密钥策略。
// @return SSHHostKeyPolicy 显式策略，或清单未填写时的安全易用默认值。
func (s SSH) EffectiveHostKeyPolicy() SSHHostKeyPolicy {
	if s.HostKeyPolicy == "" {
		return DefaultSSHHostKeyPolicy
	}
	return s.HostKeyPolicy
}

// Project 定位被备份的 docker compose 项目。
type Project struct {
	// Name 是这套服务的逻辑名称，用于快照标签。
	Name string `yaml:"name"`
	// ComposeFile 是 docker-compose.yml 在目标机上的绝对路径。
	ComposeFile string `yaml:"compose_file"`
	// EnvFile 是 compose 使用的 .env 绝对路径，可为空。
	EnvFile string `yaml:"env_file"`
	// ProjectName 对应 docker compose -p，为空时由 compose 按目录名推导。
	// 显式声明是为了避免误操作同一台机器上的另一套部署。
	ProjectName string `yaml:"project_name"`
}

// Repo 描述备份仓库（当前只支持 restic）。
//
// 全局唯一：所有机器的快照进同一个仓库，靠 host tag 区分（ADR-009）。
// 这样跨机去重才能生效——同一个基础镜像层、同样的配置文件
// 在 N 台机器上只存一份。
type Repo struct {
	// Type 目前固定为 restic。保留该字段是为了后续可能的其它后端。
	Type string `yaml:"type"`
	// URL 是 restic 仓库地址，例如 s3:https://<account>.r2.cloudflarestorage.com/<bucket>。
	// 不要按机器分路径——那会让跨机去重失效。
	URL string `yaml:"url"`
	// PasswordFile 是 restic 仓库密码文件的绝对路径。
	//
	// 这个文件必须同时存在于 hub 之外的地方（密码管理器、离线介质）。
	// 只存在于 hub 上的仓库密码，在 hub 损毁时会让所有备份一起变成废数据。
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
// 用 systemd OnCalendar 而不是进程内 cron：备份是 oneshot 进程，
// 跑完就退出。没有常驻进程就没有「守护进程自己挂了导致三个月没备份」这种故障。
// timer 现在跑在 hub 上，每台机器一个（ADR-005）。
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
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("打开清单失败: %w", err)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("清单 %s 是空文件", path)
	}

	if err := checkVersion(data); err != nil {
		return nil, err
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	// 未知字段一律报错。备份清单里的拼写错误如果被静默忽略，
	// 会让人以为某个目标已经在备份、而实际上它从未被执行过——
	// 这种错误只会在真正需要恢复的那天暴露（ADR-007）。
	dec.KnownFields(true)

	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析清单 %s 失败: %w", path, err)
	}

	cfg.path = path
	cfg.applyDefaults()
	return &cfg, nil
}

// checkVersion 在严格解析之前先判定清单版本。
//
// 顺序不能颠倒：v1 清单的顶层是 host / project / targets，
// 拿 v2 结构去严格解析只会得到一串「未知字段」，
// 而用户真正需要的信息是「这份清单该迁移了」。
func checkVersion(data []byte) error {
	var probe struct {
		Version int `yaml:"version"`
	}
	// 宽松解析：只取版本号，其余字段一律忽略。
	// 解析失败说明 YAML 语法本身有问题，此时跳过版本判定，
	// 让后面的严格解析报出带行号的语法错误。
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil
	}

	switch probe.Version {
	case SchemaVersion:
		return nil
	case 0:
		return fmt.Errorf("version: 未声明。v%d 清单必须显式写 version: %d，"+
			"参考 examples/ark.yaml", SchemaVersion, SchemaVersion)
	case 1:
		return errors.New("version: 检测到 v1 单机清单。架构已改为 hub 集中编排，" +
			"清单需要迁移到 v2（顶层改为 repo + defaults + hosts），参考 examples/ark.yaml")
	default:
		return fmt.Errorf("version: 不支持的版本 %d，当前支持 %d", probe.Version, SchemaVersion)
	}
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

// applyDefaults 只补全全局层面的默认值。
//
// 刻意不把 defaults 拷进各个 host：那会丢掉「这个值是用户写的还是继承来的」，
// 导致 on_calendar 非法时错误指向 hosts[2].schedule，
// 而用户其实写在 defaults 里。生效值由 ScheduleFor / RetentionFor 计算。
func (c *Config) applyDefaults() {
	if c.Repo.Type == "" {
		c.Repo.Type = DefaultRepoType
	}
	if c.Defaults.Schedule == nil {
		c.Defaults.Schedule = &Schedule{OnCalendar: DefaultOnCalendar}
	}
	if c.Defaults.Retention == nil {
		c.Defaults.Retention = &Retention{
			Daily:   DefaultRetentionDaily,
			Weekly:  DefaultRetentionWeekly,
			Monthly: DefaultRetentionMonthly,
		}
	}
}

// ScheduleFor 返回一台 host 实际生效的备份时机。
// 优先级：host 自身覆盖 > defaults > 内置常量。
//
// 最后一级兜底是给手工构造的 Config 用的——经过 Load 的清单
// 在 applyDefaults 阶段就已经填好了 defaults。
func (c *Config) ScheduleFor(h *Host) Schedule {
	if h != nil && h.Schedule != nil {
		return *h.Schedule
	}
	if c.Defaults.Schedule != nil {
		return *c.Defaults.Schedule
	}
	return Schedule{OnCalendar: DefaultOnCalendar}
}

// RetentionFor 返回一台 host 实际生效的保留策略。
// 优先级同 ScheduleFor。
//
// host 显式写了 retention 时完全尊重它的取值，不做任何补全——
// 「我只想留 3 天」必须被原样执行，而不是被悄悄补上周备和月备。
func (c *Config) RetentionFor(h *Host) Retention {
	if h != nil && h.Retention != nil {
		return *h.Retention
	}
	if c.Defaults.Retention != nil {
		return *c.Defaults.Retention
	}
	return Retention{
		Daily:   DefaultRetentionDaily,
		Weekly:  DefaultRetentionWeekly,
		Monthly: DefaultRetentionMonthly,
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

// ID 返回 target 在一台机器的一次快照内的唯一标识，
// 同时用作归档路径的一段。
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

	c.validateRepo(add)
	c.validateMonitoring(add)
	c.validateDNSMgr(add)
	c.validateDefaults(add)
	c.validateHosts(add)

	return errors.Join(errs...)
}

// validateDNSMgr 只校验清单中的连接字段；凭证文件和网络属于 doctor。
func (c *Config) validateDNSMgr(add func(string, ...any)) {
	if c.DNSMgr == nil {
		return
	}
	if _, err := endpoint.ParseBaseURL("dnsmgr.base_url", c.DNSMgr.BaseURL); err != nil {
		add("%v", err)
	}
	if c.DNSMgr.EnvFile == "" {
		add("dnsmgr.env_file: 不能为空")
	} else if !filepath.IsAbs(c.DNSMgr.EnvFile) {
		add("dnsmgr.env_file: 必须是绝对路径，实际 %q", c.DNSMgr.EnvFile)
	}
}

// validateMonitoring 只校验清单字段本身；文件存在性、权限和内容属于运行时与 doctor。
func (c *Config) validateMonitoring(add func(string, ...any)) {
	if c.Monitoring == nil {
		return
	}
	if c.Monitoring.EnvFile == "" {
		add("monitoring.env_file: 不能为空")
	} else if !filepath.IsAbs(c.Monitoring.EnvFile) {
		add("monitoring.env_file: 必须是绝对路径，实际 %q", c.Monitoring.EnvFile)
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

// validateDefaults 校验全局默认值。
// 两段都是可选的，只在写了的时候检查。
func (c *Config) validateDefaults(add func(string, ...any)) {
	if c.Defaults.Schedule != nil {
		validateSchedule("defaults.schedule", *c.Defaults.Schedule, add)
	}
	if c.Defaults.Retention != nil {
		validateRetention("defaults.retention", *c.Defaults.Retention, add)
	}
}

// validateHosts 逐台校验，并检查 host 名称的全局唯一性。
func (c *Config) validateHosts(add func(string, ...any)) {
	if len(c.Hosts) == 0 {
		add("hosts: 至少需要一台机器")
		return
	}

	seen := make(map[string]int, len(c.Hosts))
	for i := range c.Hosts {
		h := &c.Hosts[i]

		// 前缀带上 host 名，让错误信息在一份十几台机器的清单里可定位。
		prefix := fmt.Sprintf("hosts[%d]", i)
		if h.Host != "" {
			prefix = fmt.Sprintf("hosts[%d](%s)", i, h.Host)
		}

		switch {
		case h.Host == "":
			add("%s.host: 不能为空（它是 restic 快照的 tag，也是恢复时的检索键）", prefix)
		case !hostPattern.MatchString(h.Host):
			add("%s.host: %q 非法，只允许小写字母、数字和中划线，且不能以中划线开头或结尾", prefix, h.Host)
		default:
			// 重名会让两台机器的快照混在同一个 tag 下，恢复时无法区分。
			if prev, dup := seen[h.Host]; dup {
				add("%s.host: %q 与 hosts[%d] 重复，host 名称必须全局唯一", prefix, h.Host, prev)
			} else {
				seen[h.Host] = i
			}
		}

		validateConnection(prefix, h, add)
		validateProject(prefix, h.Project, add)
		validateTargets(prefix, h.Targets, add)
		validateHostDNSMgr(prefix+".dnsmgr", h.DNSMgr, c.DNSMgr != nil, add)

		if h.Schedule != nil {
			validateSchedule(prefix+".schedule", *h.Schedule, add)
		}
		if h.Retention != nil {
			validateRetention(prefix+".retention", *h.Retention, add)
		}
	}
}

// validateHostDNSMgr 校验 host 级维护任务、DNS 目标与记录关联。
func validateHostDNSMgr(
	prefix string,
	settings *HostDNSMgr,
	hasGlobalSettings bool,
	add func(string, ...any),
) {
	if settings == nil {
		return
	}
	if !hasGlobalSettings {
		add("%s: 已配置 dnsmgr 能力，但顶层 dnsmgr 配置缺失", prefix)
	}
	hasMaintenance := len(settings.TaskIDs) > 0
	hasDNS := strings.TrimSpace(settings.Value) != "" || len(settings.Records) > 0
	if !hasMaintenance && !hasDNS {
		add("%s: 至少需要配置 task_ids 或 value/records", prefix)
	}

	seenTasks := make(map[int64]int, len(settings.TaskIDs))
	for index, taskID := range settings.TaskIDs {
		taskPrefix := fmt.Sprintf("%s.task_ids[%d]", prefix, index)
		if taskID <= 0 {
			add("%s: 必须大于 0", taskPrefix)
		}
		if previous, exists := seenTasks[taskID]; exists {
			add("%s: 与 %s.task_ids[%d] 重复", taskPrefix, prefix, previous)
		} else {
			seenTasks[taskID] = index
		}
	}

	if !hasDNS {
		return
	}
	if net.ParseIP(strings.TrimSpace(settings.Value)) == nil {
		add("%s.value: %q 不是合法的 IPv4 或 IPv6 地址", prefix, settings.Value)
	}
	if len(settings.Records) == 0 {
		add("%s.records: 至少需要一条记录关联", prefix)
		return
	}
	seen := make(map[string]int, len(settings.Records))
	for index := range settings.Records {
		record := settings.Records[index]
		recordPrefix := fmt.Sprintf("%s.records[%d]", prefix, index)
		if record.DomainID <= 0 {
			add("%s.domain_id: 必须大于 0", recordPrefix)
		}
		if strings.TrimSpace(record.RecordID) == "" {
			add("%s.record_id: 不能为空", recordPrefix)
		}
		key := fmt.Sprintf("%d\x00%s", record.DomainID, strings.TrimSpace(record.RecordID))
		if previous, exists := seen[key]; exists {
			add("%s: 与 %s.records[%d] 重复", recordPrefix, prefix, previous)
		} else {
			seen[key] = index
		}
	}
}

// validateConnection 校验「怎么连到这台机器」，local 与 ssh 必须二选一。
func validateConnection(prefix string, h *Host, add func(string, ...any)) {
	switch {
	case h.Local && h.SSH != nil:
		add("%s: local 为 true 表示这台就是 hub 自己，命令直接本地执行，不能同时配置 ssh", prefix)
	case !h.Local && h.SSH == nil:
		add("%s.ssh: 远程机器必填（hub 需要知道怎么连过去）；"+
			"这台就是 hub 自己时请改写 local: true", prefix)
	case h.SSH != nil:
		validateSSH(prefix+".ssh", *h.SSH, add)
	}
}

// validateSSH 校验连接参数。文件是否存在、权限是否为 0600
// 属于 doctor 的职责，这里只看清单写得对不对。
func validateSSH(prefix string, s SSH, add func(string, ...any)) {
	validateAddress(prefix+".address", s.Address, add)
	if s.User == "" {
		add("%s.user: 不能为空", prefix)
	}
	if s.IdentityFile == "" {
		add("%s.identity_file: 不能为空", prefix)
	} else if !filepath.IsAbs(s.IdentityFile) {
		add("%s.identity_file: 必须是绝对路径，实际 %q", prefix, s.IdentityFile)
	}
	// 不提供「跳过主机密钥校验」的开关，所以这个字段没有缺省可言。
	if s.KnownHostsFile == "" {
		add("%s.known_hosts_file: 不能为空。生产数据会流经这条连接，"+
			"不校验主机密钥意味着中间人既能窃取数据、也能在恢复时投毒", prefix)
	} else if !filepath.IsAbs(s.KnownHostsFile) {
		add("%s.known_hosts_file: 必须是绝对路径，实际 %q", prefix, s.KnownHostsFile)
	}
	switch s.HostKeyPolicy {
	case "", SSHHostKeyPolicyAcceptNew, SSHHostKeyPolicyStrict:
	default:
		add("%s.host_key_policy: %q 非法，只允许 %q 或 %q", prefix,
			s.HostKeyPolicy, SSHHostKeyPolicyAcceptNew, SSHHostKeyPolicyStrict)
	}
}

// validateAddress 校验 host:port。
//
// net.SplitHostPort 本身太宽松，两种写法它都放行、但都连不上任何机器：
// ":22" 缺主机名，"10.0.0.11:ssh" 用了服务名（ssh 的 -p 只接受数字）。
// 这类错误必须在写清单的时候暴露——否则它会一直潜伏到某个凌晨四点，
// 由一个无人值守的定时任务替你发现。
func validateAddress(prefix, address string, add func(string, ...any)) {
	if address == "" {
		add("%s: 不能为空，格式为 host:port（例如 10.0.0.11:22）", prefix)
		return
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		add("%s: %q 不是合法的 host:port，端口必须显式写出（例如 10.0.0.11:22）", prefix, address)
		return
	}
	if host == "" {
		add("%s: %q 缺少主机名或 IP", prefix, address)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		add("%s: %q 的端口必须是 1-65535 之间的数字", prefix, address)
	}
}

// validateProject 校验 compose 项目定位信息。
func validateProject(prefix string, p Project, add func(string, ...any)) {
	if p.Name == "" {
		add("%s.project.name: 不能为空", prefix)
	}
	if p.ComposeFile == "" {
		add("%s.project.compose_file: 不能为空", prefix)
	} else if !filepath.IsAbs(p.ComposeFile) {
		add("%s.project.compose_file: 必须是绝对路径，实际 %q", prefix, p.ComposeFile)
	}
	if p.EnvFile != "" && !filepath.IsAbs(p.EnvFile) {
		add("%s.project.env_file: 必须是绝对路径，实际 %q", prefix, p.EnvFile)
	}
}

// validateSchedule 只检查非空。
// OnCalendar 的语法校验交给 doctor 调用 systemd-analyze 完成——
// 那里有 systemd 本身作为权威，比自己写一个近似的解析器可靠。
func validateSchedule(prefix string, s Schedule, add func(string, ...any)) {
	if strings.TrimSpace(s.OnCalendar) == "" {
		add("%s.on_calendar: 不能为空", prefix)
	}
}

// validateRetention 拒绝负数和「全为 0」。
func validateRetention(prefix string, r Retention, add func(string, ...any)) {
	if r.Daily < 0 || r.Weekly < 0 || r.Monthly < 0 {
		add("%s: 保留份数不能为负", prefix)
	}
	if r.Daily == 0 && r.Weekly == 0 && r.Monthly == 0 {
		add("%s: 三项不能同时为 0，否则清理时会删光所有快照", prefix)
	}
}

// validateTargets 校验一台机器上的备份目标，并检查 ID 唯一性。
//
// 唯一性是每台机器各自判断的：两台机器上有同名 volume 完全正常，
// 它们在仓库里靠 host tag 区分。
func validateTargets(prefix string, targets []Target, add func(string, ...any)) {
	if len(targets) == 0 {
		add("%s.targets: 至少需要一个备份目标", prefix)
		return
	}

	seen := make(map[string]int, len(targets))
	for i, t := range targets {
		itemPrefix := fmt.Sprintf("%s.targets[%d]", prefix, i)

		allowed, ok := allowedFields[t.Type]
		if !ok {
			add("%s.type: 未知类型 %q", itemPrefix, t.Type)
			continue
		}

		// 拒绝填在错误类型下的字段。静默忽略等于让用户以为配置生效了。
		for _, f := range t.filledFields() {
			if !allowed[f] {
				add("%s: 字段 %q 不适用于类型 %q", itemPrefix, f, t.Type)
			}
		}

		validateTargetRequired(itemPrefix, t, add)

		id := t.ID()
		if prev, dup := seen[id]; dup {
			add("%s: 与 %s.targets[%d] 重复（同一个 %q）", itemPrefix, prefix, prev, id)
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
