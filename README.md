# go-worker-demo

`go-worker-demo` 是“处理层”示例服务。

Nomad 部署时不要再用 `latest` 标签。当前示例统一使用 `go-worker-demo:dev`，否则 Docker driver 很容易继续尝试远程拉取。

它的职责是：

1. 在 Consul 中发现 `gateway-http`
2. 接收 gateway 派发的任务
3. 模拟任务执行耗时、队列、活跃任务、温度
4. 主动向 gateway 上报状态

## 主要接口

- `GET /`
- `GET /healthz`
- `GET /health`
- `GET /gateways`
- `POST /work/execute`
- `GET /metrics`

## 关键指标

- `go_worker_process_up`
- `go_worker_discovered_gateways`
- `go_worker_execute_total`
- `go_worker_execute_duration_seconds`
- `go_worker_reports_sent_total`
- `go_worker_queue_depth`
- `go_worker_active_jobs`
- `go_worker_temperature_celsius`

## 本地直跑

```bash
go mod tidy
mkdir -p ./runtime-logs
SERVICE_NAME=worker \
TARGET_SERVICE_NAME=gateway \
TARGET_DISCOVERY_SERVICE_NAME=gateway-http \
CONSUL_HTTP_ADDR=http://127.0.0.1:8500 \
APP_PORT=18081 \
METRICS_PORT=12113 \
APP_LOG_PATH=./runtime-logs/go-worker-demo.log \
go run .
```

## Loki 查询

```text
{job="go-worker-demo"}
```
