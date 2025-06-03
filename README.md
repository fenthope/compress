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
	r := touka.New()

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