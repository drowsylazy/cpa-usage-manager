package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drowsylazy/cpa-usage-manager/internal/config"
	"github.com/drowsylazy/cpa-usage-manager/internal/service"
	"github.com/drowsylazy/cpa-usage-manager/internal/store"
)

// rpcRecorder 复刻 main.go 的 rpcResponseWriter：管理 RPC 信封的接收端。
type rpcRecorder struct {
	code int
	hdr  http.Header
	buf  bytes.Buffer
}

func (w *rpcRecorder) Header() http.Header { return w.hdr }

func (w *rpcRecorder) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

func (w *rpcRecorder) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.buf.Write(p)
}

func drive(t *testing.T, a *API, method, target string, body []byte) *rpcRecorder {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{
		Method: method,
		URL:    u,
		Proto:  "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        http.Header{"Authorization": {"Bearer secret"}, "Accept-Encoding": {"gzip"}},
		Body:          http.NoBody,
		ContentLength: 0,
		Host:          u.Host,
	}
	rw := &rpcRecorder{hdr: http.Header{}}
	a.Handler().ServeHTTP(rw, req)
	return rw
}

// TestLazyGzipPlainSmallResponse 锁定小响应：无 Content-Encoding、body 即精确 JSON。
// 回归点：v0.4.0 的 lazyGzipWriter 在明文分支仍无条件 gw.Close()，
// 向 JSON 尾部追加了一整个「空 gzip 流」的二进制字节，浏览器 r.json()
// 报「Unexpected non-whitespace character after JSON」。
func TestLazyGzipPlainSmallResponse(t *testing.T) {
	st := openStore(t)
	a := New(service.New(st, defaultCfg(t), mustPeppers(t, defaultCfg(t))), st, Options{
		ManagementKey: "secret", CompressionEnabled: true, CompressionMinBytes: 1024})
	rw := drive(t, a, "GET", "/v0/management/plugins/cpa-usage-manager/usage/dimension?dimension=model&limit=50", nil)
	if ce := rw.hdr.Get("Content-Encoding"); ce != "" {
		t.Fatalf("小响应不应压缩, Content-Encoding=%q", ce)
	}
	body := rw.buf.Bytes()
	if !jsonBytesValid(body) {
		t.Fatalf("小响应 body 不是干净 JSON（尾部残留字节），尾部: %q", tailOf(body, 24))
	}
}

// TestLazyGzipLargeResponseIntegrity 锁定大响应：CE=gzip 且解压后是完整 JSON，
// 解压输出之后不得残留任何字节（用户报告的「JSON 后多出脏字符」即此类损坏）。
func TestLazyGzipLargeResponseIntegrity(t *testing.T) {
	st := openStore(t)
	svc := service.New(st, defaultCfg(t), mustPeppers(t, defaultCfg(t)))
	a := New(svc, st, Options{
		ManagementKey: "secret", CompressionEnabled: true, CompressionMinBytes: 1024})
	// 灌入足够多的 Key 让 /keys 响应超过 min_bytes(1024)，走 gzip 分支。
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		if _, err := svc.IssueKey(ctx, service.IssueRequest{
			CallerID: store.DefaultCallerID,
			Label:    strings.Repeat("k", 40) + string(rune('a'+i%26)) + string(rune(i)),
			Actor:    "t",
		}); err != nil {
			t.Fatal(err)
		}
	}
	rw := drive(t, a, "GET", "/v0/management/plugins/cpa-usage-manager/keys?limit=100", nil)
	if ce := rw.hdr.Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("大响应应为 gzip, Content-Encoding=%q len=%d", ce, rw.buf.Len())
	}
	zr, err := gzip.NewReader(bytes.NewReader(rw.buf.Bytes()))
	if err != nil {
		t.Fatalf("gzip 流无法打开（头部损坏）: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip 流读取失败（流被截断或污染）: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("gzip 收尾异常: %v", err)
	}
	if !jsonBytesValid(plain) {
		t.Fatalf("解压后的 body 不是干净 JSON，尾部字节: %q", tailOf(plain, 16))
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	c := defaultCfg(t)
	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(c.DataDir, c.DatabaseFile), OwnerID: "gz"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func defaultCfg(t *testing.T) config.Config {
	t.Helper()
	c := config.Default()
	c.DataDir = t.TempDir()
	c.DatabaseFile = "gz.db"
	return c
}

func mustPeppers(t *testing.T, c config.Config) service.PepperSet {
	t.Helper()
	ps, err := service.LoadPeppers(c, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// jsonBytesValid 报告 b 是否为「单值 JSON 后仅允许空白」——与浏览器 r.json() 同判据。
func jsonBytesValid(b []byte) bool {
	d := json.NewDecoder(bytes.NewReader(b))
	var v any
	if err := d.Decode(&v); err != nil {
		return false
	}
	if _, err := d.Token(); err != io.EOF {
		return false
	}
	return true
}

func tailOf(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
