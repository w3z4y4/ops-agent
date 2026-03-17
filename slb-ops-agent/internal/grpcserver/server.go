package grpcserver

import (
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/yourorg/slb-ops-agent/internal/config"
	"github.com/yourorg/slb-ops-agent/internal/logger"
	"github.com/yourorg/slb-ops-agent/internal/security"
	pb "github.com/yourorg/slb-ops-agent/pkg/proto"
)

// Server 封装 gRPC 服务器生命周期。
type Server struct {
	grpcSrv *grpc.Server
	lis     net.Listener
}

// NewServer 构建带 mTLS + IP-ACL + 审计拦截器的 gRPC 服务器。
func NewServer(cfg *config.Config, handler *Handler, audit *logger.AuditLogger) (*Server, error) {
	// ── mTLS 凭证 ───────────────────────────────────────────────────────────
	tlsOpt, err := security.NewServerTLSCredentials(&cfg.TLS)
	if err != nil {
		return nil, fmt.Errorf("build TLS credentials: %w", err)
	}

	// ── IP 白名单解析 ────────────────────────────────────────────────────────
	allowedNets, err := security.ParseCIDRs(cfg.ACL.AllowedCIDRs)
	if err != nil {
		return nil, fmt.Errorf("parse allowed CIDRs: %w", err)
	}

	// ── gRPC Server ─────────────────────────────────────────────────────────
	grpcSrv := grpc.NewServer(
		tlsOpt,
		grpc.MaxRecvMsgSize(cfg.GRPC.MaxRecvMsgSize),
		grpc.MaxSendMsgSize(cfg.GRPC.MaxSendMsgSize),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * 60, // 5 分钟空闲断开
			Time:              60,
			Timeout:           10,
		}),
		grpc.ChainUnaryInterceptor(
			security.NewUnaryInterceptors(allowedNets, audit),
		),
		grpc.ChainStreamInterceptor(
			security.NewStreamInterceptors(allowedNets),
		),
	)

	pb.RegisterAgentServiceServer(grpcSrv, handler)

	// ── 监听 ─────────────────────────────────────────────────────────────────
	lis, err := net.Listen("tcp", cfg.GRPC.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", cfg.GRPC.ListenAddr, err)
	}

	return &Server{grpcSrv: grpcSrv, lis: lis}, nil
}

// Serve 开始接受连接（阻塞）。
func (s *Server) Serve() error {
	return s.grpcSrv.Serve(s.lis)
}

// GracefulStop 等待当前请求处理完成后停止。
func (s *Server) GracefulStop() {
	s.grpcSrv.GracefulStop()
}
