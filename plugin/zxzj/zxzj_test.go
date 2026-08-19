package zxzj

import "testing"

func TestParsePlayerData(t *testing.T) {
	p := &ZXZJPlugin{}
	body := []byte(`var player_aaaa={"url":"https://pan.baidu.com/s/example?pwd=zxzj","from":"baidu"};`)
	link, password := p.parsePlayerData(body)
	if link != "https://pan.baidu.com/s/example?pwd=zxzj" {
		t.Fatalf("link = %q", link)
	}
	if password != "zxzj" {
		t.Fatalf("password = %q, want zxzj", password)
	}
}

func TestBuildAbsURLUsesCurrentDomain(t *testing.T) {
	p := &ZXZJPlugin{}
	got := p.buildAbsURL("/voddetail/4623.html")
	want := baseURL + "/voddetail/4623.html"
	if got != want {
		t.Fatalf("url = %q, want %q", got, want)
	}
}
