package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
)

// ComposeMetadata 保存恢复计划需要、且不会泄漏 Compose 环境值的结构化元数据。
type ComposeMetadata struct {
	// PublishedPorts 是规范化后的全部宿主机 published port 声明。
	PublishedPorts []PublishedPort `json:"published_ports"`
}

// PublishedPort 描述备份时 Compose 中的一条 published port 声明。
type PublishedPort struct {
	// Service 是 Compose service 名。
	Service string `json:"service"`
	// HostIP 是宿主机绑定地址，可为空。
	HostIP string `json:"host_ip,omitempty"`
	// Published 是原宿主机端口；原配置由 Docker 动态分配时为空。
	Published string `json:"published,omitempty"`
	// Target 是容器端口。
	Target uint16 `json:"target"`
	// Protocol 是 tcp 或 udp。
	Protocol string `json:"protocol"`
	// AppProtocol 是 Compose 可选应用协议提示。
	AppProtocol string `json:"app_protocol,omitempty"`
	// Mode 是 Compose 端口发布模式，可为空。
	Mode string `json:"mode,omitempty"`
}

func parseComposeMetadata(canonical []byte) (*ComposeMetadata, error) {
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	var document struct {
		Services map[string]struct {
			Ports []map[string]any `json:"ports"`
		} `json:"services"`
	}
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("Compose canonical JSON 无效: %w", err)
	}
	if document.Services == nil {
		return nil, fmt.Errorf("Compose canonical JSON 缺少 services")
	}

	metadata := &ComposeMetadata{PublishedPorts: []PublishedPort{}}
	services := make([]string, 0, len(document.Services))
	for service := range document.Services {
		services = append(services, service)
	}
	sort.Strings(services)
	for _, service := range services {
		for _, rawPort := range document.Services[service].Ports {
			port, err := publishedPortFromCompose(service, rawPort)
			if err != nil {
				return nil, err
			}
			metadata.PublishedPorts = append(metadata.PublishedPorts, port)
		}
	}
	if err := validateComposeMetadata(*metadata); err != nil {
		return nil, err
	}
	return metadata, nil
}

func validateComposeMetadata(metadata ComposeMetadata) error {
	for index, port := range metadata.PublishedPorts {
		if strings.TrimSpace(port.Service) == "" || port.Target == 0 || strings.TrimSpace(port.Protocol) == "" {
			return fmt.Errorf("published_ports[%d] 的 service、target 或 protocol 无效", index)
		}
		if port.HostIP != "" && net.ParseIP(port.HostIP) == nil {
			return fmt.Errorf("published_ports[%d] 的 host_ip %q 无效", index, port.HostIP)
		}
		if port.Published != "" {
			published, err := strconv.ParseUint(port.Published, 10, 16)
			if err != nil || published == 0 {
				return fmt.Errorf("published_ports[%d] 的 published %q 无效", index, port.Published)
			}
		}
	}
	return nil
}

func publishedPortFromCompose(service string, value map[string]any) (PublishedPort, error) {
	target, err := parseComposePortNumber(value["target"])
	if err != nil || target == 0 {
		return PublishedPort{}, fmt.Errorf("Compose service %q port target 无效", service)
	}
	protocol := strings.TrimSpace(composeStringValue(value["protocol"]))
	if protocol == "" {
		protocol = "tcp"
	}
	return PublishedPort{
		Service:     service,
		HostIP:      strings.TrimSpace(composeStringValue(value["host_ip"])),
		Published:   strings.TrimSpace(composeStringValue(value["published"])),
		Target:      target,
		Protocol:    protocol,
		AppProtocol: strings.TrimSpace(composeStringValue(value["app_protocol"])),
		Mode:        strings.TrimSpace(composeStringValue(value["mode"])),
	}, nil
}

func composeStringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func parseComposePortNumber(value any) (uint16, error) {
	parsed, err := strconv.ParseUint(composeStringValue(value), 10, 16)
	return uint16(parsed), err
}

func cloneComposeMetadata(value *ComposeMetadata) *ComposeMetadata {
	if value == nil {
		return nil
	}
	return &ComposeMetadata{PublishedPorts: append([]PublishedPort(nil), value.PublishedPorts...)}
}
