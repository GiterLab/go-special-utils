# S3Store

此包从 <https://github.com/tus/tusd> 工程的 e6bb8ec 版本中分离出来，并做了部分修改以适应本项目的需求。

保留了 s3store 使用 github.com/aws/aws-sdk-go/aws 作为 AWS SDK 的实现。

因为 github.com/aws/aws-sdk-go-v2/aws 版本的 SDK 对操作早期版本的 minio (minio:v2020.12.23 已验证不兼容) 实例存在不兼容问题。
