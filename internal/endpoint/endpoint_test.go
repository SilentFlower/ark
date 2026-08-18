package endpoint

import "testing"

func TestParseHTTPSOrLoopback(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "HTTPS", value: "https://dns.example.com/api"},
		{name: "IPv4 loopback", value: "http://127.0.0.1:8080"},
		{name: "localhost", value: "http://localhost:8080"},
		{name: "非 loopback HTTP", value: "http://dns.example.com", wantErr: true},
		{name: "userinfo", value: "https://user@dns.example.com", wantErr: true},
		{name: "fragment", value: "https://dns.example.com/#secret", wantErr: true},
		{name: "相对 URL", value: "/api", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseHTTPSOrLoopback("endpoint", tc.value)
			if (err != nil) != tc.wantErr {
				t.Fatalf("错误 = %v，wantErr=%t", err, tc.wantErr)
			}
		})
	}
}

func TestParseBaseURL_拒绝查询参数(t *testing.T) {
	if _, err := ParseBaseURL("endpoint", "https://dns.example.com/root?token=secret"); err == nil {
		t.Fatal("基础 URL 不应接受查询参数")
	}
	if _, err := ParseBaseURL("endpoint", "https://dns.example.com/root"); err != nil {
		t.Fatalf("带路径前缀的基础 URL 应通过: %v", err)
	}
}
