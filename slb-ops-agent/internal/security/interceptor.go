package security

import (
	"context"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/yourorg/slb-ops-agent/internal/logger"
)

// ─────────────────────────────────────────────
//  Unary 拦截器链
// ─────────────────────────────────────────────

// NewUnaryInterceptors 组合 IP-ACL 拦截器与审计日志拦截器。
func NewUnaryInterceptors(
	allowedNets []*net.IPNet,
	audit *logger.AuditLogger,
) grpc.UnaryServerInterceptor {
	return chainUnary(
		unaryIPACL(allowedNets),
		unaryAudit(audit),
	)
}

// NewStreamInterceptors 组合流式 IP-ACL 拦截器。
func NewStreamInterceptors(allowedNets []*net.IPNet) grpc.StreamServerInterceptor {
	return streamIPACL(allowedNets)
}

// ─────────────────────────────────────────────
//  IP ACL
// ─────────────────────────────────────────────

func unaryIPACL(nets []*net.IPNet) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {

		if err := checkIP(ctx, nets); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func streamIPACL(nets []*net.IPNet) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo,
		handler grpc.StreamHandler) error {

		if err := checkIP(ss.Context(), nets); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func checkIP(ctx context.Context, nets []*net.IPNet) error {
	if len(nets) == 0 {
		return nil
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.PermissionDenied, "cannot determine caller IP")
	}
	ip := ExtractPeerIP(p)
	if !CheckIPAllowed(ip, nets) {
		return status.Errorf(codes.PermissionDenied, "IP %s not in allowed list", ip)
	}
	return nil
}

// ─────────────────────────────────────────────
//  Audit Logging
// ─────────────────────────────────────────────

func unaryAudit(audit *logger.AuditLogger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {

		start := time.Now()

		p, _ := peer.FromContext(ctx)
		callerIP := "unknown"
		if p != nil {
			callerIP = ExtractPeerIP(p)
		}

		resp, err := handler(ctx, req)

		entry := logger.AuditEntry{
			Timestamp:  start,
			CallerIP:   callerIP,
			Method:     info.FullMethod,
			Params:     summarizeRequest(req),
			ExitCode:   -1,
			DurationMS: time.Since(start).Milliseconds(),
		}
		if err != nil {
			entry.Result = "error"
			entry.ErrorDetail = err.Error()
		} else {
			entry.Result = "success"
		}

		audit.Log(entry)
		return resp, err
	}
}

// summarizeRequest 提取请求中的关键字段用于审计，避免记录敏感大数据。
func summarizeRequest(req any) map[string]any {
	// 通过类型开关只摘取关键字段
	// 使用反射或手动各类型判断均可；这里留 TODO 由具体模块扩展。
	return map[string]any{"type": fmt.Sprintf("%T", req)}
}

// ─────────────────────────────────────────────
//  链式组合工具
// ─────────────────────────────────────────────

func chainUnary(interceptors ...grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (any, error) {

		chain := handler
		for i := len(interceptors) - 1; i >= 0; i-- {
			next := chain
			idx := i
			chain = func(c context.Context, r any) (any, error) {
				return interceptors[idx](c, r, info, next)
			}
		}
		return chain(ctx, req)
	}
}
