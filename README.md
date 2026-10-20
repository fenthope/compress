# compress

Touka框架的压缩中间件, 支持deflate gzip zstd

## 安装

```bash
go get github.com/fenthope/compress
```

## 使用示例

```go
func main() {
	// 1. 创建 Touka 路由Engine
	r := touka.Default()

	r.Use(compress.Compression(compress.CompressOptions{
		// Algorithms: 配置每种压缩算法的级别和是否启用对象池
		Algorithms: map[string]compress.AlgorithmConfig{
			compress.EncodingGzip: {
				Level:       gzip.BestCompression, // Gzip最高压缩比
				PoolEnabled: true,                 // 启用Gzip压缩器的对象池
			},
			compress.EncodingDeflate: {
				Level:       flate.DefaultCompression, // Deflate默认压缩比
				PoolEnabled: false,                    // Deflate不启用对象池
			},
			compress.EncodingZstd: {
				Level:       int(zstd.SpeedBestCompression), // Zstandard最佳压缩比
				PoolEnabled: true,                           // 启用Zstandard压缩器的对象池
			},
		},

		// MinContentLength: 响应内容达到此字节数才进行压缩 (例如 1KB)
		MinContentLength: 1024,

		// CompressibleTypes: 只有响应的 Content-Type 匹配此列表中的MIME类型前缀才进行压缩
		CompressibleTypes: []string{
			"text/",          // 匹配所有 text/* 类型
			"application/json",
			"image/svg+xml",
		},

		// EncodingPriority: 当客户端接受多种支持的压缩算法时，服务器选择的优先级顺序
		EncodingPriority: []string{
			compress.EncodingZstd,
			compress.EncodingGzip,
			compress.EncodingDeflate,
		},
	}))
}
```

## 压缩级别与对象池

- `Algorithms[enc].Level` 的取值：
  - gzip / deflate：标准库压缩级别（`HuffmanOnly(-2)`、`DefaultCompression(-1)`、`NoCompression(0)`、`BestSpeed(1)`..`BestCompression(9)`）。
  - zstd：`int(zstd.SpeedFastest|SpeedDefault|SpeedBetterCompression|SpeedBestCompression)`（枚举值 1..4）。
- `PoolEnabled: true` 时，压缩器会按级别从对象池取用并归还，避免每个响应都新建编码器
  （gzip 默认级别约 1MB/编码器，不池化时每请求多约 1MB 分配）。
- 池槽位覆盖上述全部级别；zstd 只对四个标准档位池化，其余级别直接新建编码器。
