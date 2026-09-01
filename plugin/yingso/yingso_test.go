package yingso

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	jsonutil "pansou/util/json"
)

func TestXORUTF16RoundTrip(t *testing.T) {
	original := `{"title":"仙逆 Movie"}`
	key := "cdef"
	encrypted := xorUTF16(original, key)
	if decrypted := xorUTF16(encrypted, key); decrypted != original {
		t.Fatalf("unexpected decrypted string: %q", decrypted)
	}
}

func TestBuildLink(t *testing.T) {
	tests := []struct {
		root     int
		key      string
		linkType string
		linkURL  string
		password string
	}{
		{root: 1, key: "R2SMZBmfyni", linkType: "aliyun", linkURL: "https://www.aliyundrive.com/s/R2SMZBmfyni"},
		{root: 2, key: "8c76976bb12f", linkType: "quark", linkURL: "https://pan.quark.cn/s/8c76976bb12f"},
		{root: 3, key: "VNwTcQi?pwd=c6w3", linkType: "xunlei", linkURL: "https://pan.xunlei.com/s/VNwTcQi?pwd=c6w3", password: "c6w3"},
		{root: 4, key: "1abc?pwd=8888", linkType: "baidu", linkURL: "https://pan.baidu.com/s/1abc?pwd=8888", password: "8888"},
		{root: 5, key: "b2bf7111919e4", linkType: "uc", linkURL: "https://drive.uc.cn/s/b2bf7111919e4"},
	}

	for _, test := range tests {
		linkType, linkURL, password := buildLink(test.root, test.key)
		if linkType != test.linkType || linkURL != test.linkURL || password != test.password {
			t.Fatalf("root %d: got %q %q %q", test.root, linkType, linkURL, password)
		}
	}
}

func TestSearchImpl(t *testing.T) {
	config := apiConfig{URLVersion: "v1", UserID: "test-user", Start: 2, End: 6}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/test":
			encodedConfig, err := jsonutil.MarshalString(config)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeJSON(t, w, apiEnvelope[string]{Code: 200, Msg: "ok", Data: xorUTF16(encodedConfig, bootstrapKey)})
		case "/v1/search":
			var payload searchPayload
			decodeRequest(t, r, config, &payload)
			if payload.Title != "仙逆" || payload.UserID != config.UserID || payload.Root != 0 {
				t.Errorf("unexpected search payload: %#v", payload)
			}
			writeJSON(t, w, apiEnvelope[[]searchItem]{Code: 200, Msg: "ok", Data: []searchItem{
				{ID: 101, Title: "仙逆 4K\t", Root: 2},
				{ID: 102, Title: "仙逆 百度", Root: 4},
				{ID: 103, Title: "仙逆 未知", Root: 99},
			}})
		case "/v1/getKey":
			var payload getKeyPayload
			decodeRequest(t, r, config, &payload)
			keys := map[int64]string{
				101: "quark-share",
				102: "baidu-share?pwd=1234",
				103: "unsupported",
			}
			writeJSON(t, w, apiEnvelope[string]{Code: 200, Msg: "ok", Data: keys[payload.ID]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	p := NewYingsoPlugin()
	p.apiBaseURL = server.URL
	results, err := p.searchImpl(server.Client(), " 仙逆 ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d: %#v", len(results), results)
	}
	if results[0].UniqueID != "yingso-101" || results[0].Title != "仙逆 4K" {
		t.Fatalf("unexpected first result: %#v", results[0])
	}
	if results[0].Links[0].URL != "https://pan.quark.cn/s/quark-share" || results[0].Links[0].WorkTitle != "仙逆 4K" {
		t.Fatalf("unexpected first link: %#v", results[0].Links[0])
	}
	if results[1].Links[0].Type != "baidu" || results[1].Links[0].Password != "1234" {
		t.Fatalf("unexpected second link: %#v", results[1].Links[0])
	}
}

func TestLiveSearch(t *testing.T) {
	if os.Getenv("YINGSO_LIVE_TEST") != "1" {
		t.Skip("set YINGSO_LIVE_TEST=1 to query yingso.fun")
	}

	keyword := os.Getenv("YINGSO_LIVE_KEYWORD")
	if keyword == "" {
		keyword = "仙逆"
	}
	p := NewYingsoPlugin()
	results, err := p.searchImpl(&http.Client{Timeout: 15 * time.Second}, keyword, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("live search returned no results")
	}
	if len(results[0].Links) == 0 || results[0].Links[0].URL == "" {
		t.Fatalf("live search returned an invalid first result: %#v", results[0])
	}
	t.Logf("live search for %q returned %d results; first result: %q (%s)", keyword, len(results), results[0].Title, results[0].Links[0].URL)
}

func decodeRequest(t *testing.T, r *http.Request, config apiConfig, target interface{}) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		return
	}
	var encrypted encryptedPayload
	if err := jsonutil.Unmarshal(body, &encrypted); err != nil {
		t.Error(err)
		return
	}
	if len(encrypted.No) != 24 {
		t.Errorf("unexpected no length: %d", len(encrypted.No))
		return
	}
	key := encrypted.No[config.Start:config.End]
	if err := jsonutil.UnmarshalString(xorUTF16(encrypted.Info, key), target); err != nil {
		t.Error(err)
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value interface{}) {
	t.Helper()
	data, err := jsonutil.Marshal(value)
	if err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if _, err := fmt.Fprint(w, string(data)); err != nil && !strings.Contains(err.Error(), "closed") {
		t.Error(err)
	}
}
