package grpcserver

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yourorg/slb-ops-agent/internal/executor"
	"github.com/yourorg/slb-ops-agent/internal/filemanager"
	"github.com/yourorg/slb-ops-agent/internal/health"
	"github.com/yourorg/slb-ops-agent/internal/servicectl"
	"github.com/yourorg/slb-ops-agent/internal/upgrade"
	pb "github.com/yourorg/slb-ops-agent/pkg/proto"
)

// Handler 实现 pb.AgentServiceServer，聚合所有子模块。
type Handler struct {
	pb.UnimplementedAgentServiceServer

	exec    *executor.Executor
	files   *filemanager.Manager
	svcCtl  *servicectl.Controller
	prober  *health.Prober
	upgrader *upgrade.Manager
}

// New 创建 Handler 并注入所有依赖。
func New(
	exec *executor.Executor,
	files *filemanager.Manager,
	svcCtl *servicectl.Controller,
	prober *health.Prober,
	upgrader *upgrade.Manager,
) *Handler {
	return &Handler{
		exec:     exec,
		files:    files,
		svcCtl:   svcCtl,
		prober:   prober,
		upgrader: upgrader,
	}
}

// ─────────────────────────────────────────────
//  ExecuteCommand（同步）
// ─────────────────────────────────────────────

func (h *Handler) ExecuteCommand(ctx context.Context, req *pb.CommandRequest) (*pb.CommandResponse, error) {
	if req.Command == "" {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}

	result := h.exec.Execute(executor.Request{
		Command:    req.Command,
		Args:       req.Args,
		TimeoutSec: int(req.TimeoutSec),
		Env:        req.Env,
	})

	return &pb.CommandResponse{
		Output:     result.Output,
		ExitCode:   int32(result.ExitCode),
		Error:      result.Error,
		DurationMs: result.DurationMS,
	}, nil
}

// ─────────────────────────────────────────────
//  ExecuteCommandAsync
// ─────────────────────────────────────────────

func (h *Handler) ExecuteCommandAsync(ctx context.Context, req *pb.CommandRequest) (*pb.AsyncCommandResponse, error) {
	if req.Command == "" {
		return nil, status.Error(codes.InvalidArgument, "command is required")
	}

	taskID := h.exec.ExecuteAsync(executor.Request{
		Command:    req.Command,
		Args:       req.Args,
		TimeoutSec: int(req.TimeoutSec),
		Env:        req.Env,
	})

	return &pb.AsyncCommandResponse{TaskId: taskID}, nil
}

// ─────────────────────────────────────────────
//  GetTaskResult
// ─────────────────────────────────────────────

func (h *Handler) GetTaskResult(ctx context.Context, req *pb.TaskResultRequest) (*pb.CommandResponse, error) {
	result, ok := h.exec.GetResult(req.TaskId)
	if !ok {
		return &pb.CommandResponse{
			TaskId: req.TaskId,
			Error:  "task pending or not found",
		}, nil
	}

	return &pb.CommandResponse{
		TaskId:     result.TaskID,
		Output:     result.Output,
		ExitCode:   int32(result.ExitCode),
		Error:      result.Error,
		DurationMs: result.DurationMS,
	}, nil
}

// ─────────────────────────────────────────────
//  UploadFile（流式接收）
// ─────────────────────────────────────────────

func (h *Handler) UploadFile(stream pb.AgentService_UploadFileServer) error {
	var transferID string
	var destPath string
	initialized := false

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if transferID != "" {
				h.files.AbortUpload(transferID)
			}
			return status.Errorf(codes.Internal, "recv error: %v", err)
		}

		// 首个 chunk 初始化会话
		if !initialized {
			transferID = chunk.TransferId
			destPath = chunk.DestPath

			if transferID == "" || destPath == "" {
				return status.Error(codes.InvalidArgument, "transfer_id and dest_path required in first chunk")
			}

			uid := int(chunk.Uid)
			if chunk.Uid == 0 {
				uid = -1 // 不修改
			}
			gid := int(chunk.Gid)
			if chunk.Gid == 0 {
				gid = -1
			}

			if err := h.files.StartUpload(transferID, destPath, uid, gid, chunk.Mode); err != nil {
				return status.Errorf(codes.Internal, "start upload: %v", err)
			}
			initialized = true
		}

		// 写入数据块
		if len(chunk.Data) > 0 {
			if err := h.files.WriteChunk(transferID, chunk.Data); err != nil {
				h.files.AbortUpload(transferID)
				return status.Errorf(codes.Internal, "write chunk: %v", err)
			}
		}

		// 最后一块：校验并落盘
		if chunk.IsLastChunk {
			if err := h.files.FinalizeUpload(transferID, chunk.Sha256Total); err != nil {
				return status.Errorf(codes.DataLoss, "finalize upload: %v", err)
			}
			break
		}
	}

	return stream.SendAndClose(&pb.TransferStatus{
		Success:    true,
		TransferId: transferID,
		DestPath:   destPath,
	})
}

// ─────────────────────────────────────────────
//  DownloadFile（流式发送）
// ─────────────────────────────────────────────

func (h *Handler) DownloadFile(req *pb.DownloadRequest, stream pb.AgentService_DownloadFileServer) error {
	if req.SrcPath == "" {
		return status.Error(codes.InvalidArgument, "src_path is required")
	}

	chunks, errs := filemanager.StreamDownload(req.SrcPath, int(req.ChunkSize))
	seqNum := int32(0)

	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return nil // 正常结束
			}
			seqNum++
			pbChunk := &pb.FileChunk{
				ChunkId:     fmt.Sprintf("%d", seqNum),
				Data:        chunk.Data,
				IsLastChunk: chunk.IsLastChunk,
				Sha256Total: chunk.SHA256Total,
			}
			if err := stream.Send(pbChunk); err != nil {
				return status.Errorf(codes.Internal, "send chunk: %v", err)
			}

		case err, ok := <-errs:
			if ok && err != nil {
				return status.Errorf(codes.Internal, "read file: %v", err)
			}
		}
	}
}

// ─────────────────────────────────────────────
//  ManageService
// ─────────────────────────────────────────────

func (h *Handler) ManageService(ctx context.Context, req *pb.ServiceRequest) (*pb.ServiceResponse, error) {
	if req.ServiceName == "" {
		return nil, status.Error(codes.InvalidArgument, "service_name is required")
	}

	action := protoActionToLocal(req.Action)
	if action == "" {
		return nil, status.Error(codes.InvalidArgument, fmt.Sprintf("unknown action: %v", req.Action))
	}

	result := h.svcCtl.Execute(req.ServiceName, action)

	resp := &pb.ServiceResponse{
		Success:     result.Success,
		ServiceName: result.ServiceName,
		ActiveState: result.ActiveState,
		SubState:    result.SubState,
		Output:      result.Output,
		Error:       result.Error,
	}
	return resp, nil
}

func protoActionToLocal(a pb.ServiceAction) servicectl.Action {
	switch a {
	case pb.ServiceAction_START:
		return servicectl.ActionStart
	case pb.ServiceAction_STOP:
		return servicectl.ActionStop
	case pb.ServiceAction_RESTART:
		return servicectl.ActionRestart
	case pb.ServiceAction_RELOAD:
		return servicectl.ActionReload
	case pb.ServiceAction_STATUS:
		return servicectl.ActionStatus
	case pb.ServiceAction_ENABLE:
		return servicectl.ActionEnable
	case pb.ServiceAction_DISABLE:
		return servicectl.ActionDisable
	default:
		return ""
	}
}

// ─────────────────────────────────────────────
//  GetHealthStatus
// ─────────────────────────────────────────────

func (h *Handler) GetHealthStatus(ctx context.Context, _ *pb.HealthRequest) (*pb.HealthResponse, error) {
	snap := h.prober.Latest()

	return &pb.HealthResponse{
		Timestamp: snap.Timestamp.Unix(),
		Agent: &pb.AgentHealth{
			MemoryBytes:    snap.Agent.MemoryBytes,
			CpuPercent:     snap.Agent.CPUPercent,
			GoroutineCount: int32(snap.Agent.GoroutineCount),
			UptimeSec:      snap.Agent.UptimeSec,
			Version:        snap.Agent.Version,
		},
		Haproxy:      svcStatusToProto(snap.HAProxy),
		Confd:        svcStatusToProto(snap.Confd),
		NodeExporter: svcStatusToProto(snap.NodeExporter),
	}, nil
}

func svcStatusToProto(s health.ServiceStatus) *pb.ServiceHealth {
	return &pb.ServiceHealth{
		Name:              s.Name,
		ProcessAlive:      s.ProcessAlive,
		PortOpen:          s.PortOpen,
		ListenPort:        int32(s.ListenPort),
		ActiveState:       s.ActiveState,
		ConfigCheckOutput: s.ConfigCheckOutput,
		ConfigValid:       s.ConfigValid,
	}
}

// ─────────────────────────────────────────────
//  UpgradeAgent
// ─────────────────────────────────────────────

func (h *Handler) UpgradeAgent(ctx context.Context, req *pb.UpgradeRequest) (*pb.UpgradeResponse, error) {
	if req.DownloadUrl == "" || req.Sha256Hash == "" || req.TargetVersion == "" {
		return nil, status.Error(codes.InvalidArgument, "download_url, sha256_hash and target_version are required")
	}

	err := h.upgrader.StartUpgrade(upgrade.UpgradeRequest{
		DownloadURL:        req.DownloadUrl,
		SHA256Hash:         req.Sha256Hash,
		TargetVersion:      req.TargetVersion,
		ValidateTimeoutSec: int(req.ValidateTimeoutSec),
	})
	if err != nil {
		return &pb.UpgradeResponse{Error: err.Error()}, nil
	}

	return &pb.UpgradeResponse{Accepted: true}, nil
}

// ─────────────────────────────────────────────
//  Heartbeat
// ─────────────────────────────────────────────

func (h *Handler) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
	// 通知升级管理器：新版本与控制台握手成功
	h.upgrader.OnHeartbeatSuccess()

	return &pb.HeartbeatResponse{
		Ok:        true,
		Timestamp: time.Now().Unix(),
	}, nil
}
