# go-special-utils

一些特殊的 Go 库包，主要服务于 IoT 平台的文件与对象存储场景。

## 包一览

| 包 | 说明 |
| :--- | :--- |
| [`pkg/tuscore`](./pkg/tuscore) | tusd `DataStore` 核心实现：分片聚合、`.part` 断点恢复、完成与终止。由两个 provider 共用，不含任何云厂商 SDK。 |
| [`pkg/miniostore`](./pkg/miniostore) | MinIO / S3 的 `tuscore.Bucket` 实现，基于 [`minio-go`](https://github.com/minio/minio-go)。 |
| [`pkg/ossstore`](./pkg/ossstore) | 阿里云 OSS 的 `tuscore.Bucket` 实现，基于 [`aliyun-oss-go-sdk`](https://github.com/aliyun/aliyun-oss-go-sdk)。 |
| [`pkg/s3store`](./pkg/s3store) | 旧版 S3Store fork（AWS SDK v1），用于兼容早期 MinIO。**新项目请使用 miniostore。** |

`tuscore` + `miniostore` / `ossstore` 不依赖 AWS SDK，因此不受上游 `s3store` 的
checksum、`aws-chunked`、`Content-MD5` 等兼容性问题影响。

## 环境要求

- Go 1.25.14 及以上
- `github.com/tus/tusd/v2 v2.10.1`（仅使用 handler / hooks / locker）
- `pkg/miniostore`：`github.com/minio/minio-go/v7`
- `pkg/ossstore`：`github.com/aliyun/aliyun-oss-go-sdk`

## 快速开始

```go
import (
    "github.com/tus/tusd/v2/pkg/handler"
    "github.com/tus/tusd/v2/pkg/memorylocker"

    "github.com/GiterLab/go-special-utils/pkg/miniostore"
)

store, err := miniostore.New(miniostore.Config{
    Endpoint:  "172.16.11.36:9000",
    AccessKey: "minioadmin",
    SecretKey: "minioadmin",
    Bucket:    "files",
    Prefix:    "iotda/upload/tus/",
    Secure:    false,
})
if err != nil {
    return err
}

composer := handler.NewStoreComposer()
store.UseIn(composer) // 同时注册 DataStore 与 Terminater
composer.UseLocker(memorylocker.New())

tusHandler, err := handler.NewHandler(handler.Config{
    BasePath:      "/files/",
    StoreComposer: composer,
})
```

OSS 只需替换构造函数（endpoint 不含 bucket）：

```go
store, err := ossstore.New(ossstore.Config{
    Endpoint:  "oss-cn-hangzhou.aliyuncs.com",
    AccessKey: os.Getenv("OSS_AK"),
    SecretKey: os.Getenv("OSS_SK"),
    Bucket:    "files",
    Prefix:    "iotda/upload/tus/",
    Secure:    true,
})
```

## 存储布局与语义

| Key | 内容 |
| :--- | :--- |
| `<prefix><objectID>` | 内容对象（完成后存在） |
| `<prefix><objectID>.info` | 上传元数据（JSON，`handler.FileInfo`） |
| `<prefix><objectID>.part` | 未满一个分片的尾部数据 |

- 分片最小 5 MiB（`MinPartSize`），默认按 50 MiB 聚合；文件较大时会按 10000 片上限自动放大分片。
- `GetInfo` 的 offset 由 `ListParts` 之和加上 `.part` 大小计算，进程重启后可直接续传，客户端不会重复传输已落盘的数据。
- `FinishUpload` 会把 `.part` 提升为最后一个分片后完成；零字节上传走单对象写入。
- 终止上传会 `AbortMultipartUpload` 并删除 `.info`、`.part` 与内容对象，不依赖任何批量删除接口。
- 上传 ID 为 `<objectID>+<provider uploadID>`，调用方如需记录主键，请取 `+` 之前的部分。

## 与上游 s3store 的差异

- 不使用 AWS SDK，签名、校验和与传输编码完全由 minio-go / aliyun-oss-sdk 处理。
- 不提供 `DeleteObjects` 批量删除，规避 MinIO/OSS 对 `Content-MD5` 的差异。
- 并发控制仍由 tusd `Locker` 决定：默认 `memorylocker` 仅适用于单实例部署，多副本需要替换为分布式锁。

## 测试

```bash
go test ./...
```

- `pkg/tuscore`：内存 fake bucket，覆盖小文件、多分片、重启续传、零字节、终止与元数据恢复。
- `pkg/s3store`：保留上游原有的 mock 测试。

## 版本记录

- **unreleased**
  - 新增 `pkg/tuscore`、`pkg/miniostore`、`pkg/ossstore`。
  - `tusd/v2` 升级到 `v2.10.1`（包含 deferred-length 截断与终止竞态修复）。
  - `pkg/s3store` 保持原有行为，仅随依赖升级。
