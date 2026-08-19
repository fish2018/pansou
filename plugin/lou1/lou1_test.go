package lou1

import "testing"

func TestClassifyLinkRecognizesTorrentAttachment(t *testing.T) {
	typ, link := classifyLink("attach-download-17436.htm")
	if typ != "others" {
		t.Fatalf("type = %q, want others", typ)
	}
	if link != baseURL+"/attach-download-17436.htm" {
		t.Fatalf("link = %q, want %q", link, baseURL+"/attach-download-17436.htm")
	}
}
