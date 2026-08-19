package yunso

import "testing"

func TestDecryptYunsoURLAcceptsPlainURL(t *testing.T) {
	const shareURL = "https://pan.quark.cn/s/example"
	got, err := decryptYunsoURL(shareURL)
	if err != nil {
		t.Fatalf("decryptYunsoURL returned error: %v", err)
	}
	if got != shareURL {
		t.Fatalf("decryptYunsoURL = %q, want %q", got, shareURL)
	}
}
