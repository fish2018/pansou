package miosou

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseAndSolveAnubisChallenge(t *testing.T) {
	body := []byte(`<html><script id="anubis_challenge" type="application/json">{"rules":{"algorithm":"fast","difficulty":3},"challenge":{"id":"challenge-1","randomData":"abc123"}}</script></html>`)
	challenge, found, err := parseAnubisChallenge(body)
	if err != nil {
		t.Fatalf("parse challenge: %v", err)
	}
	if !found || challenge.Challenge.ID != "challenge-1" || challenge.Rules.Difficulty != 3 {
		t.Fatalf("challenge = %+v, found = %v", challenge, found)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	hash, nonce, err := solveAnubisChallenge(ctx, challenge.Challenge.RandomData, challenge.Rules.Difficulty)
	if err != nil {
		t.Fatalf("solve challenge: %v", err)
	}
	wantHash := sha256.Sum256([]byte(challenge.Challenge.RandomData + strconv.FormatUint(nonce, 10)))
	if hash != hex.EncodeToString(wantHash[:]) || !hasLeadingZeroNibbles(wantHash[:], challenge.Rules.Difficulty) {
		t.Fatalf("invalid proof: hash=%s nonce=%d", hash, nonce)
	}
}

func TestParseSearchStreamMergesSnapshots(t *testing.T) {
	input := "event: snapshot\ndata: {\"merged_by_type\":{\"quark\":[{\"url\":\"https://pan.quark.cn/s/abc\",\"note\":\"资源一\"}]}}\n\n" +
		"event: snapshot\ndata: {\"merged_by_type\":{\"quark\":[{\"url\":\"https://pan.quark.cn/s/abc\",\"note\":\"资源一更新\"},{\"share_ref\":\"ref-1\",\"note\":\"资源二\"}],\"baidu\":[{\"url\":\"https://pan.baidu.com/s/1x?pwd=abcd\",\"password\":\"abcd\"}]}}\n\n" +
		"event: done\ndata: {\"total\":3}\n\n"
	groups, err := parseSearchStream(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse stream: %v", err)
	}
	if got := len(groups["quark"]); got != 2 {
		t.Fatalf("quark item count = %d, want 2", got)
	}
	if got := groups["quark"][0].Note; got != "资源一更新" {
		t.Fatalf("updated note = %q", got)
	}
	if got := len(groups["baidu"]); got != 1 {
		t.Fatalf("baidu item count = %d, want 1", got)
	}
}

func TestParseSearchStreamReturnsErrorEvent(t *testing.T) {
	_, err := parseSearchStream(strings.NewReader("event: error\ndata: {\"message\":\"访问频繁\"}\n\n"))
	if err == nil || !strings.Contains(err.Error(), "访问频繁") {
		t.Fatalf("error = %v, want upstream message", err)
	}
}

func TestDetectLinkTypeAndPassword(t *testing.T) {
	url := "https://pan.baidu.com/s/1abc?pwd=a1b2"
	if got := detectLinkType(url); got != "baidu" {
		t.Fatalf("link type = %q, want baidu", got)
	}
	if got := extractPassword(url); got != "a1b2" {
		t.Fatalf("password = %q, want a1b2", got)
	}
	if got := extractPassword("资源名称 提取码：z9x8"); got != "z9x8" {
		t.Fatalf("note password = %q, want z9x8", got)
	}
}

func TestMergeItemsUsesStreamKey(t *testing.T) {
	items := mergeItems(nil, []searchItem{{StreamKey: "same", Note: "first", ShareRef: "ref-1"}})
	items = mergeItems(items, []searchItem{{StreamKey: "same", Note: "updated"}})
	if len(items) != 1 {
		t.Fatalf("item count = %d, want 1", len(items))
	}
	if items[0].Note != "updated" || items[0].ShareRef != "ref-1" {
		t.Fatalf("merged item = %+v", items[0])
	}
}
