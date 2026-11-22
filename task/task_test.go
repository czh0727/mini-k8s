package task

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// 使用内存中的 HTTP transport 验证 SDK 请求，无须本机运行 Docker。
func newTestClient(t *testing.T, handler func(*http.Request) (int, string)) *client.Client {
	t.Helper()
	cli, err := client.New(
		client.WithHost("http://docker.test"),
		client.WithAPIVersion("1.57"),
		client.WithHTTPClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			status, body := handler(req)
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestRunContainerLifecycle(t *testing.T) {
	port := network.MustParsePort("8080/tcp")
	var requests []string
	cli := newTestClient(t, func(req *http.Request) (int, string) {
		requests = append(requests, req.Method+" "+req.URL.Path)
		switch req.URL.Path {
		case "/v1.57/images/create":
			return http.StatusOK, `{"status":"Pull complete"}`
		case "/v1.57/containers/create":
			var body struct {
				container.Config
				HostConfig container.HostConfig
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if req.URL.Query().Get("name") != "test-task" || body.Image != "alpine:latest" {
				t.Errorf("unexpected container name or image: %s, %s", req.URL, body.Image)
			}
			if strings.Join(body.Cmd, " ") != "echo hello" || strings.Join(body.Env, " ") != "APP=test" {
				t.Errorf("command or environment not forwarded: %+v", body.Config)
			}
			if !body.AttachStdout || !body.AttachStderr || body.Tty {
				t.Errorf("unexpected stream configuration: %+v", body.Config)
			}
			if _, ok := body.ExposedPorts[port]; !ok || !body.HostConfig.PublishAllPorts {
				t.Error("exposed ports or publish-all-ports setting not forwarded")
			}
			if body.HostConfig.NanoCPUs != 500_000_000 || body.HostConfig.Memory != 64*1024*1024 {
				t.Errorf("unexpected resource limits: %+v", body.HostConfig.Resources)
			}
			if body.HostConfig.RestartPolicy.Name != container.RestartPolicyOnFailure {
				t.Errorf("unexpected restart policy: %+v", body.HostConfig.RestartPolicy)
			}
			return http.StatusCreated, `{"Id":"test-container"}`
		case "/v1.57/containers/test-container/start":
			return http.StatusNoContent, ""
		case "/v1.57/containers/test-container/logs":
			if req.URL.Query().Get("stdout") != "1" || req.URL.Query().Get("stderr") != "1" || req.URL.Query().Has("follow") {
				t.Errorf("unexpected log options: %s", req.URL)
			}
			return http.StatusOK, ""
		default:
			t.Errorf("unexpected request: %s %s", req.Method, req.URL)
			return http.StatusNotFound, `{"message":"unexpected request"}`
		}
	})
	d := Docker{Client: cli, Config: Config{
		Name:          "test-task",
		Image:         "alpine:latest",
		Cmd:           []string{"echo", "hello"},
		Env:           []string{"APP=test"},
		AttachStdout:  true,
		AttachStderr:  true,
		ExportedPorts: network.PortSet{port: {}},
		Cpu:           0.5,
		Memory:        64 * 1024 * 1024,
		RestartPolicy: "on-failure",
	}}
	result := d.Run()
	if result.Error != nil || result.ContainerId != "test-container" || result.Action != "start" || result.Result != "success" {
		t.Fatalf("unexpected result: %+v", result)
	}
	want := "POST /v1.57/images/create,POST /v1.57/containers/create,POST /v1.57/containers/test-container/start,GET /v1.57/containers/test-container/logs"
	if got := strings.Join(requests, ","); got != want {
		t.Errorf("request sequence = %s; want %s", got, want)
	}
}

func TestRunStopsOnPullStreamError(t *testing.T) {
	var requests int
	cli := newTestClient(t, func(req *http.Request) (int, string) {
		requests++
		return http.StatusOK, `{"errorDetail":{"message":"pull denied"},"error":"pull denied"}`
	})
	d := Docker{Client: cli, Config: Config{Image: "private:latest"}}
	result := d.Run()
	if result.Error == nil || !strings.Contains(result.Error.Error(), "pull denied") {
		t.Fatalf("expected pull stream error, got %+v", result)
	}
	if requests != 1 {
		t.Errorf("container operation attempted after failed pull: %d requests", requests)
	}
}

func TestStopContainerLifecycle(t *testing.T) {
	for _, stopFails := range []bool{false, true} {
		name := "success"
		if stopFails {
			name = "stop failure prevents removal"
		}
		t.Run(name, func(t *testing.T) {
			var requests []string
			cli := newTestClient(t, func(req *http.Request) (int, string) {
				requests = append(requests, req.Method+" "+req.URL.Path)
				if len(requests) == 1 {
					if req.URL.Query().Has("t") {
						t.Error("stop should use the engine's default timeout")
					}
					if stopFails {
						return http.StatusInternalServerError, `{"message":"stop failed"}`
					}
				} else if req.URL.Query().Get("v") != "1" || req.URL.Query().Has("force") {
					t.Errorf("unexpected remove options: %s", req.URL)
				}
				return http.StatusNoContent, ""
			})
			d := Docker{Client: cli}
			result := d.Stop("test-container")
			want := "POST /v1.57/containers/test-container/stop"
			if stopFails {
				if result.Error == nil || result.Result != "" {
					t.Fatalf("expected stop failure, got %+v", result)
				}
			} else {
				want += ",DELETE /v1.57/containers/test-container"
				if result.Error != nil || result.Result != "success" {
					t.Fatalf("unexpected stop result: %+v", result)
				}
			}
			if result.Action != "stop" || result.ContainerId != "test-container" {
				t.Errorf("missing operation or container ID: %+v", result)
			}
			if got := strings.Join(requests, ","); got != want {
				t.Errorf("request sequence = %s; want %s", got, want)
			}
		})
	}
}
