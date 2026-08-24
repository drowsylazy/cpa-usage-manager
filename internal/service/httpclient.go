package service

import (
	"net"
	"net/http"
	"time"
)

// sharedTransport 是外部 HTTP 调用共用的连接池：TLS 握手与 TCP 连接跨调用复用，
// 避免每次请求新建 Transport 造成的重复握手。各调用方按需包一层 *http.Client
// 设定自己的超时。
var sharedTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:          16,
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: time.Second,
}
