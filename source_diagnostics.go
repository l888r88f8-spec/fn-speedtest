package main

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
)

// Only controlled labels and counts are returned to the browser, never URLs,
// credentials, directory payloads, or raw upstream error text.
type sourceDiagnostic struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	Message    string         `json:"message"`
	Candidates int            `json:"candidates"`
	Tested     int            `json:"tested"`
	Available  int            `json:"available"`
	Failures   map[string]int `json:"failures,omitempty"`
}
type globalDiscoveryResult struct {
	Servers    []serverOption
	Diagnostic sourceDiagnostic
}
type globalStageFailure struct {
	stage string
	err   error
}

func (e *globalStageFailure) Error() string     { return e.stage + "失败" }
func (e *globalStageFailure) Unwrap() error     { return e.err }
func globalStage(stage string, err error) error { return &globalStageFailure{stage: stage, err: err} }

type globalHTTPFailure int

func (e globalHTTPFailure) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

type globalIssue string

func (e globalIssue) Error() string { return string(e) }
func globalFailureReason(err error) string {
	var issue globalIssue
	if errors.As(err, &issue) {
		return string(issue)
	}
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typeErr) {
		return "目录数据格式不匹配"
	}
	var status globalHTTPFailure
	if errors.As(err, &status) {
		return status.Error()
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "域名解析失败"
	}
	var invalid x509.CertificateInvalidError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	if errors.As(err, &invalid) || errors.As(err, &unknown) || errors.As(err, &hostname) {
		return "证书校验失败"
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &network) && network.Timeout()) {
		return "连接超时"
	}
	if errors.Is(err, context.Canceled) {
		return "请求中止"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "连接被拒绝"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "连接被重置"
	}
	if errors.Is(err, syscall.EPIPE) {
		return "连接已关闭"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "响应提前结束"
	}
	if errors.Is(err, io.EOF) {
		return "服务器提前关闭连接"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "server gave http response to https client"):
		return "HTTPS 节点返回了 HTTP 响应"
	case strings.Contains(message, "tls"):
		return "TLS 握手失败"
	case strings.Contains(message, "malformed http"):
		return "HTTP 响应格式异常"
	case strings.Contains(message, "connection reset"):
		return "连接被重置"
	case strings.Contains(message, "connection refused"):
		return "连接被拒绝"
	}
	return "连接或响应异常"
}
func globalPhase(err error) string {
	var stage *globalStageFailure
	if errors.As(err, &stage) {
		return stage.stage
	}
	return "校验"
}
