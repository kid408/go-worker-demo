# go-worker-demo

`go-worker-demo` 现在是 gRPC 处理层。

职责：

1. 发现 `gateway-grpc`
2. 对外提供 `WorkerService/ProcessSessionEvent`
3. 处理 `login / heartbeat / logout`
4. 登录时将会话快照写入 MinIO
5. 周期性向 gateway 发送 gRPC 状态上报

它保留了 HTTP 端口，只做：

- `/healthz`
- `/gateways`

真正的业务通信已经改成 gRPC。

## 默认端口

- HTTP：`18081`
- gRPC：`19081`
- Metrics：`12113`

## 关键环境变量

- `CONSUL_HTTP_ADDR`
- `TARGET_DISCOVERY_SERVICE_NAME=gateway-grpc`
- `APP_PORT`
- `GRPC_PORT`
- `METRICS_PORT`
- `MINIO_ENDPOINT`
- `MINIO_ACCESS_KEY`
- `MINIO_SECRET_KEY`
- `MINIO_BUCKET`

## 本地运行

```powershell
go mod tidy
$env:CONSUL_HTTP_ADDR="http://127.0.0.1:8500"
$env:TARGET_DISCOVERY_SERVICE_NAME="gateway-grpc"
$env:APP_PORT="18081"
$env:GRPC_PORT="19081"
$env:METRICS_PORT="12113"
$env:MINIO_ENDPOINT="127.0.0.1:9000"
$env:MINIO_ACCESS_KEY="minioadmin"
$env:MINIO_SECRET_KEY="minioadmin123"
$env:MINIO_BUCKET="login-snapshots"
go run .
```
