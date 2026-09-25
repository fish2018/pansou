package config

import "testing"

// 上游证书校验默认必须开启。有插件（qqpd、panyq）原先硬编码 InsecureSkipVerify: true，
// 等于把返回内容对所有中间人开放；"某插件需要"不等于"该默认开着"。
func TestAllowInsecureTLSDefaultsToFalse(t *testing.T) {
	t.Setenv("INSECURE_SKIP_TLS_VERIFY", "")
	if getInsecureSkipTLSVerify() {
		t.Error("未设置环境变量时必须默认校验证书")
	}

	saved := AppConfig
	AppConfig = nil
	t.Cleanup(func() { AppConfig = saved })
	if AllowInsecureTLS() {
		t.Error("配置未初始化时应退回安全默认（校验）")
	}
}

func TestAllowInsecureTLSExplicitOptIn(t *testing.T) {
	for _, v := range []string{"true", "1"} {
		t.Setenv("INSECURE_SKIP_TLS_VERIFY", v)
		if !getInsecureSkipTLSVerify() {
			t.Errorf("INSECURE_SKIP_TLS_VERIFY=%q 应被识别为开启", v)
		}
	}
	for _, v := range []string{"false", "0", "yes", "TRUE "} {
		t.Setenv("INSECURE_SKIP_TLS_VERIFY", v)
		if getInsecureSkipTLSVerify() {
			t.Errorf("INSECURE_SKIP_TLS_VERIFY=%q 不该被识别为开启", v)
		}
	}
}
