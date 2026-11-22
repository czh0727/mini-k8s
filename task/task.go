package task

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// Docker 使用调用方初始化的 SDK 客户端执行容器操作；客户端也由调用方关闭。
type Docker struct {
	Client *client.Client
	Config Config
}

// DockerResult 保存操作结果。容器创建后即返回其 ID，后续步骤失败时也可据此清理。
type DockerResult struct {
	Error       error
	Action      string
	ContainerId string
	Result      string
}

type State int

const (
	Pending State = iota
	Scheduled
	Running
	Completed
	Failed
)

// Config 描述容器的启动配置。
type Config struct {
	Name         string
	AttachStdin  bool
	AttachStdout bool
	AttachStderr bool
	// 新版 SDK 使用 network.PortSet，可用 network.ParsePort("8080/tcp") 构造端口。
	ExportedPorts network.PortSet
	Cmd           []string
	Image         string
	Cpu           float64 // CPU 核数，例如 0.5 表示半核；0 表示不限制。
	Memory        int64   // 内存上限，单位为字节；0 表示不限制。
	Disk          int64   // 预留字段，当前尚未应用磁盘配额。
	Env           []string
	RestartPolicy string
}

type TaskEvent struct {
	ID        uuid.UUID
	State     State
	Timestamp time.Time
	Task      Task
}

type Task struct {
	ID            uuid.UUID
	Name          string
	State         State
	Image         string
	Memory        int
	Disk          int
	ExposedPort   network.PortSet
	PortBinding   map[string]string
	RestartPolicy string
	StartTime     time.Time
	EndTime       time.Time
}

// Run 拉取镜像、创建并启动容器，输出当前日志后返回，不等待容器退出。
func (d *Docker) Run() DockerResult {
	result := DockerResult{Action: "start"}
	if d.Client == nil {
		result.Error = fmt.Errorf("docker client is not initialized")
		return result
	}
	ctx := context.Background()

	log.Printf("Pulling image %s", d.Config.Image)
	reader, err := d.Client.ImagePull(ctx, d.Config.Image, client.ImagePullOptions{})
	if err != nil {
		result.Error = fmt.Errorf("pull image %s: %w", d.Config.Image, err)
		return result
	}
	// ImagePull 返回响应流时镜像可能尚未拉完。Wait 会读取到结束并检查
	// JSON 消息中的拉取错误（即使 HTTP 状态为 200），同时关闭响应流。
	if err := reader.Wait(ctx); err != nil {
		result.Error = fmt.Errorf("wait for image %s: %w", d.Config.Image, err)
		return result
	}

	cc := &container.Config{
		Image:        d.Config.Image,
		Cmd:          d.Config.Cmd,
		Env:          d.Config.Env,
		AttachStdin:  d.Config.AttachStdin,
		AttachStdout: d.Config.AttachStdout,
		AttachStderr: d.Config.AttachStderr,
		Tty:          false,
		ExposedPorts: d.Config.ExportedPorts,
	}
	hc := &container.HostConfig{
		RestartPolicy: container.RestartPolicy{
			Name: container.RestartPolicyMode(d.Config.RestartPolicy),
		},
		Resources: container.Resources{
			Memory: d.Config.Memory,
			// Docker 的 NanoCPUs 以十亿分之一核为单位：1 核 = 1e9。
			NanoCPUs: int64(d.Config.Cpu * 1e9),
		},
		// 为暴露的容器端口分配宿主机随机端口，不代表宿主机使用同一端口号。
		PublishAllPorts: true,
	}

	// 新版 SDK 将原先的多个位置参数收拢到 ContainerCreateOptions。
	resp, err := d.Client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     cc,
		HostConfig: hc,
		Name:       d.Config.Name,
	})
	if err != nil {
		result.Error = fmt.Errorf("create container using image %s: %w", d.Config.Image, err)
		return result
	}
	result.ContainerId = resp.ID

	// 启动、停止、删除接口现在都返回 (Result, error)；无须使用结果时用 _ 接收。
	if _, err := d.Client.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		result.Error = fmt.Errorf("start container %s: %w", resp.ID, err)
		return result
	}

	out, err := d.Client.ContainerLogs(ctx, resp.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     false, // 只取当前日志，避免长时间运行的服务阻塞 Run。
	})
	if err != nil {
		result.Error = fmt.Errorf("get logs for container %s: %w", resp.ID, err)
		return result
	}
	defer out.Close()

	// Tty=false 时日志带有 Docker 多路复用头，须用 StdCopy 拆分 stdout/stderr。
	if _, err := stdcopy.StdCopy(os.Stdout, os.Stderr, out); err != nil {
		result.Error = fmt.Errorf("read logs for container %s: %w", resp.ID, err)
		return result
	}
	result.Result = "success"
	return result
}

// Stop 先优雅停止容器，成功后删除容器及其匿名卷。
func (d *Docker) Stop(id string) DockerResult {
	result := DockerResult{Action: "stop", ContainerId: id}
	if d.Client == nil {
		result.Error = fmt.Errorf("docker client is not initialized")
		return result
	}
	ctx := context.Background()
	// Timeout 留空，使用容器自身配置或 Docker Engine 默认的停止超时。
	if _, err := d.Client.ContainerStop(ctx, id, client.ContainerStopOptions{}); err != nil {
		result.Error = fmt.Errorf("stop container %s: %w", id, err)
		return result
	}
	if _, err := d.Client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         false,
	}); err != nil {
		result.Error = fmt.Errorf("remove container %s: %w", id, err)
		return result
	}
	result.Result = "success"
	return result
}
