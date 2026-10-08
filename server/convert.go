package server

import (
	"context"
	"time"

	"github.com/containerd/containerd"
	serverv1 "github.com/hostinger/fireactions/proto/server/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// convertPoolToProto converts a Pool to its protobuf representation.
func convertPoolToProto(ctx context.Context, pool *Pool) *serverv1.Pool {
	state := serverv1.PoolState_POOL_STATE_ACTIVE
	if !pool.IsActive() {
		state = serverv1.PoolState_POOL_STATE_PAUSED
	}

	return &serverv1.Pool{
		Name:            pool.config.Name,
		Organization:    pool.config.Runner.Organization,
		Replicas:        int32(pool.GetReplicas()),
		CurrentReplicas: int32(pool.GetCurrentSize()),
		DesiredReplicas: int32(pool.GetDesired()),
		GroupId:         pool.config.Runner.GroupID,
		Labels:          pool.config.Runner.Labels,
		Image:           pool.config.Runner.Image,
		State:           state,
	}
}

func convertMachineToProto(ctx context.Context, machine VM) *serverv1.Machine {
	info := machine.Info()
	m := &serverv1.Machine{
		ID:        info.Name,
		Pool:      info.Pool,
		Addr:      info.Addr,
		CreatedAt: timestamppb.New(info.CreatedAt),
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	m.RunnerState, _ = machine.RunnerState(ctx)
	m.RunnerVersion, _ = machine.RunnerVersion(ctx)

	return m
}

// convertImageToProto converts a containerd Image to its protobuf representation.
func convertImageToProto(ctx context.Context, img containerd.Image) *serverv1.Image {
	size, _ := img.Size(ctx)
	createdAt := img.Metadata().CreatedAt

	i := &serverv1.Image{
		Name:      img.Name(),
		Size:      size,
		CreatedAt: timestamppb.New(createdAt),
	}

	return i
}
