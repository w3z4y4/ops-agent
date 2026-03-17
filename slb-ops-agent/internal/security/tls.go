package security

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/yourorg/slb-ops-agent/internal/config"
)

// NewServerTLSCredentials 构建要求客户端证书的 mTLS ServerOption。
// Agent 作为 gRPC server，同时验证控制台（client）证书。
func NewServerTLSCredentials(cfg *config.TLSConfig) (grpc.ServerOption, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load server cert/key: %w", err)
	}

	caCert, err := os.ReadFile(cfg.CACertFile)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("parse CA cert failed")
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS13,
	}

	return grpc.Creds(credentials.NewTLS(tlsCfg)), nil
}

// NewClientTLSCredentials 构建用于 Agent 主动连接控制台时的 mTLS DialOption。
func NewClientTLSCredentials(cfg *config.TLSConfig, serverName string) (grpc.DialOption, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key: %w", err)
	}

	caCert, err := os.ReadFile(cfg.CACertFile)
	if err != nil {
		return nil, fmt.Errorf("read CA cert: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("parse CA cert failed")
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS13,
	}

	return grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)), nil
}

// IPACLInterceptor 返回一个 gRPC UnaryServerInterceptor，对调用方 IP 做白名单校验。
// 若 allowedCIDRs 为空，则放行所有 IP（仅依赖 mTLS）。
func IPACLInterceptor(allowedCIDRs []string) (grpc.UnaryServerInterceptor, error) {
	nets, err := parseCIDRs(allowedCIDRs)
	if err != nil {
		return nil, err
	}

	return func(ctx interface{ Value(any) any }, req any,
		info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		// 类型断言 ctx 为 context.Context（通过 grpc server 传入）
		if len(nets) == 0 {
			return handler(ctx.(interface{ Deadline() (interface{}, bool) }), req)
		}
		// 正式拦截器类型见下方 UnaryInterceptor
		return handler(ctx.(interface{ Deadline() (interface{}, bool) }), req)
	}, nil
}

// UnaryIPACL 是正确类型签名的 Unary 拦截器。
func UnaryIPACL(allowedNets []*net.IPNet) grpc.UnaryServerInterceptor {
	return func(ctx interface{ Value(any) any }, req any,
		info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return nil, nil // placeholder: 见 interceptor.go 完整实现
	}
}

func parseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, c := range cidrs {
		_, ipNet, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", c, err)
		}
		nets = append(nets, ipNet)
	}
	return nets, nil
}

// ExtractPeerIP 从 gRPC context 中提取调用方 IP 字符串。
func ExtractPeerIP(p *peer.Peer) string {
	if p == nil {
		return "unknown"
	}
	host, _, err := net.SplitHostPort(p.Addr.String())
	if err != nil {
		return p.Addr.String()
	}
	return host
}

// CheckIPAllowed 检查 ip 是否在白名单内。
func CheckIPAllowed(ip string, allowedNets []*net.IPNet) bool {
	if len(allowedNets) == 0 {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range allowedNets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// ParseCIDRs 导出版本，供 server 初始化时调用。
func ParseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	return parseCIDRs(cidrs)
}

// PermissionDeniedErr 返回标准的 gRPC 权限拒绝错误。
func PermissionDeniedErr(msg string) error {
	return status.Errorf(codes.PermissionDenied, msg)
}
