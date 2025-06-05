package compress

import (
	"bufio"
	"compress/gzip" // Gzip
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/flate" // Deflate

	"github.com/infinite-iroha/touka"
	"github.com/klauspost/compress/zstd" // Zstandard
)

// HTTP 头部常量
const (
	headerAcceptEncoding  = "Accept-Encoding"  // 客户端接受的编码
	headerContentEncoding = "Content-Encoding" // 响应使用的编码
	headerContentLength   = "Content-Length"   // 内容长度
	headerContentType     = "Content-Type"     // 内容类型
	headerVary            = "Vary"             // 缓存控制
)

// 支持的压缩编码名称
const (
	encodingGzip     = "gzip"
	encodingDeflate  = "deflate"
	encodingZstd     = "zstd"
	encodingIdentity = "identity" // 表示不压缩
)

// defaultCompressibleTypes 默认可压缩的 MIME 类型列表
var defaultCompressibleTypes = []string{
	"text/html", "text/css", "text/plain", "text/javascript",
	"application/javascript", "application/x-javascript", "application/json",
	"application/xml", "image/svg+xml",
	"application/font-woff", "application/font-woff2",
	"application/x-font-woff", "application/x-font-woff2",
	"application/vnd.ms-fontobject",
	"image/x-icon",
	"image/bmp",
	"image/jpeg",
	"image/png",
	"image/gif",
	"image/webp",
}

// AlgorithmConfig 用于特定压缩算法的配置
type AlgorithmConfig struct {
	// Level 是压缩级别。具体含义取决于算法：
	// - Gzip: gzip.BestSpeed (-2) 到 gzip.BestCompression (9), gzip.DefaultCompression (-1)
	// - Deflate: flate.BestSpeed (-2) 到 flate.BestCompression (9), flate.DefaultCompression (-1)
	// - Zstd: zstd.SpeedFastest (1) 到 zstd.SpeedBestCompression (22 approx), zstd.SpeedDefault (3)
	Level int
	// PoolEnabled 指示是否为此算法和级别启用对象池。
	// 对于不常用的级别或算法，可以禁用池以减少内存占用。
	PoolEnabled bool
}

// CompressOptions 用于配置压缩中间件
type CompressOptions struct {
	// Algorithms 是一个映射，键是编码名称 (如 "gzip", "zstd")，值是该算法的配置。
	// 中间件会根据此映射中存在的算法及其在 Accept-Encoding 中的 q 值来选择。
	// 如果此映射为空，则默认启用 Gzip (DefaultCompression) 和 Deflate (DefaultCompression)。
	// Zstd 默认不启用，除非显式配置。
	// 用户可以通过此配置禁用某些算法或设置其级别。
	Algorithms map[string]AlgorithmConfig

	// MinContentLength 是应用压缩的最小内容长度 (字节)。
	// 如果响应的 Content-Length 小于此值，则不应用压缩。默认为 0 (无最小限制)。
	MinContentLength int64

	// CompressibleTypes 是要压缩的 MIME 类型列表。
	// 如果为空，将使用 defaultCompressibleTypes。
	CompressibleTypes []string

	// EncodingPriority 是一个有序的编码名称切片，用于在客户端支持多种可用算法时决定优先级。
	// 例如：[]string{"zstd", "gzip", "deflate"}。
	// 如果为空，默认优先级为：zstd (如果已配置), gzip, deflate。
	EncodingPriority []string
}

// compressWriter 接口定义了压缩写入器需要实现的方法。
// 它嵌入了 io.WriteCloser，并添加了 Flush 方法。
type compressWriter interface {
	io.WriteCloser
	Flush() error
	Reset(w io.Writer) // 重置写入器以重用，并关联新的底层写入器
}

// --- gzip specific writer and pool ---
type gzipCompressWriter struct {
	*gzip.Writer
	level int // 用于归还到正确的池
}

func (gzw *gzipCompressWriter) Reset(w io.Writer) { gzw.Writer.Reset(w) }
func (gzw *gzipCompressWriter) Flush() error      { return gzw.Writer.Flush() } // gzip.Writer.Flush() returns error

var gzipWriterPoolsArray [gzip.BestCompression - gzip.BestSpeed + 3]*sync.Pool // +1 for DefaultCompression, +1 for 0 index

func initGzipPools() {
	for i := gzip.BestSpeed; i <= gzip.BestCompression; i++ {
		level := i
		idx := level - gzip.BestSpeed // Map level to 0-based index for positive levels
		if level == gzip.DefaultCompression {
			idx = gzip.BestCompression - gzip.BestSpeed + 1 // Special index for DefaultCompression
		} else if level == 0 { // gzip.NoCompression is 0
			idx = gzip.BestCompression - gzip.BestSpeed + 2 // Special index for NoCompression
		}

		gzipWriterPoolsArray[idx] = &sync.Pool{
			New: func() interface{} {
				// 初始化时 writer 为 nil
				w, _ := gzip.NewWriterLevel(nil, level)
				return &gzipCompressWriter{Writer: w, level: level}
			},
		}
	}
}

// --- deflate specific writer and pool ---
type deflateCompressWriter struct {
	*flate.Writer
	level int
}

func (fw *deflateCompressWriter) Reset(w io.Writer) { fw.Writer.Reset(w) }
func (fw *deflateCompressWriter) Flush() error      { return fw.Writer.Flush() }

var deflateWriterPoolsArray [flate.BestCompression - flate.BestSpeed + 2]*sync.Pool // +1 for DefaultCompression

func initDeflatePools() {
	for i := flate.BestSpeed; i <= flate.BestCompression; i++ {
		level := i
		idx := level - flate.BestSpeed
		if level == flate.DefaultCompression {
			idx = flate.BestCompression - flate.BestSpeed + 1
		}
		deflateWriterPoolsArray[idx] = &sync.Pool{
			New: func() interface{} {
				w, _ := flate.NewWriter(nil, level)
				return &deflateCompressWriter{Writer: w, level: level}
			},
		}
	}
}

// --- zstd specific writer and pool ---
// zstd 级别较多，这里简化为只池化默认级别或用户指定的少数级别
// 为了更通用，我们可以基于 AlgorithmConfig 中的 Level 动态创建池，或者只池化常见的。
// 这里我们先为默认级别创建一个池。
type zstdCompressWriter struct {
	*zstd.Encoder
	level zstd.EncoderLevel // zstd.EncoderLevel 是一个类型别名
}

// zstd.Encoder 的 Reset 方法签名是 Reset(dst io.Writer) error
// 为了适配 compressWriter 接口，我们需要一个包装
func (zw *zstdCompressWriter) Reset(w io.Writer)                 { zw.Encoder.Reset(w) } // 忽略 Reset 的错误，或记录它
func (zw *zstdCompressWriter) Flush() error                      { return zw.Encoder.Flush() }
func (zw *zstdCompressWriter) Close() error                      { return zw.Encoder.Close() }
func (zw *zstdCompressWriter) Write(p []byte) (n int, err error) { return zw.Encoder.Write(p) }

var zstdWriterPoolDefault *sync.Pool

func initZstdPools() {
	// 默认池化 zstd.SpeedDefault 级别
	zstdWriterPoolDefault = &sync.Pool{
		New: func() interface{} {
			// zstd.WithWindowSize(1<<20) // 1MB window, example option
			w, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
			return &zstdCompressWriter{Encoder: w, level: zstd.SpeedDefault}
		},
	}
}

func init() {
	initGzipPools()
	initDeflatePools()
	initZstdPools()
}

// getCompressor 从池中获取或创建一个新的压缩器
func getCompressor(encoding string, level int, underlyingWriter io.Writer, poolEnabled bool) compressWriter {
	switch encoding {
	case encodingGzip:
		idx := level - gzip.BestSpeed
		if level == gzip.DefaultCompression {
			idx = gzip.BestCompression - gzip.BestSpeed + 1
		}
		if level == 0 {
			idx = gzip.BestCompression - gzip.BestSpeed + 2
		}
		if poolEnabled && idx >= 0 && idx < len(gzipWriterPoolsArray) && gzipWriterPoolsArray[idx] != nil {
			cw := gzipWriterPoolsArray[idx].Get().(*gzipCompressWriter)
			cw.Reset(underlyingWriter)
			return cw
		}
		// 如果池未启用或级别超出预设池范围，则创建新的
		w, _ := gzip.NewWriterLevel(underlyingWriter, level)
		return &gzipCompressWriter{Writer: w, level: level}
	case encodingDeflate:
		idx := level - flate.BestSpeed
		if level == flate.DefaultCompression {
			idx = flate.BestCompression - flate.BestSpeed + 1
		}
		if poolEnabled && idx >= 0 && idx < len(deflateWriterPoolsArray) && deflateWriterPoolsArray[idx] != nil {
			cw := deflateWriterPoolsArray[idx].Get().(*deflateCompressWriter)
			cw.Reset(underlyingWriter)
			return cw
		}
		w, _ := flate.NewWriter(underlyingWriter, level)
		return &deflateCompressWriter{Writer: w, level: level}
	case encodingZstd:
		// 简化：仅当级别为 SpeedDefault 且池启用时才使用池
		// 生产代码中可以为特定需要的 zstd 级别创建更多池
		zstdLevel := zstd.EncoderLevelFromZstd(level) // 将 int 转换为 zstd.EncoderLevel
		if poolEnabled && zstdLevel == zstd.SpeedDefault && zstdWriterPoolDefault != nil {
			cw := zstdWriterPoolDefault.Get().(*zstdCompressWriter)
			cw.Reset(underlyingWriter)
			return cw
		}
		w, _ := zstd.NewWriter(underlyingWriter, zstd.WithEncoderLevel(zstdLevel))
		return &zstdCompressWriter{Encoder: w, level: zstdLevel}
	}
	return nil
}

// putCompressor 将压缩器返还到相应的池中
func putCompressor(cw compressWriter, encoding string, poolEnabled bool) {
	if !poolEnabled || cw == nil {
		return
	}
	switch encoding {
	case encodingGzip:
		if gzw, ok := cw.(*gzipCompressWriter); ok {
			idx := gzw.level - gzip.BestSpeed
			if gzw.level == gzip.DefaultCompression {
				idx = gzip.BestCompression - gzip.BestSpeed + 1
			}
			if gzw.level == 0 {
				idx = gzip.BestCompression - gzip.BestSpeed + 2
			}
			if idx >= 0 && idx < len(gzipWriterPoolsArray) && gzipWriterPoolsArray[idx] != nil {
				gzipWriterPoolsArray[idx].Put(gzw)
			}
		}
	case encodingDeflate:
		if fw, ok := cw.(*deflateCompressWriter); ok {
			idx := fw.level - flate.BestSpeed
			if fw.level == flate.DefaultCompression {
				idx = flate.BestCompression - flate.BestSpeed + 1
			}
			if idx >= 0 && idx < len(deflateWriterPoolsArray) && deflateWriterPoolsArray[idx] != nil {
				deflateWriterPoolsArray[idx].Put(fw)
			}
		}
	case encodingZstd:
		if zw, ok := cw.(*zstdCompressWriter); ok {
			if zw.level == zstd.SpeedDefault && zstdWriterPoolDefault != nil { // 仅返还默认级别的到池
				zstdWriterPoolDefault.Put(zw)
			}
		}
	}
}

// compressResponseWriter 包装了 touka.ResponseWriter 以提供多种压缩功能
type compressResponseWriter struct {
	touka.ResponseWriter                // 底层的 ResponseWriter
	compressor           compressWriter // 当前使用的压缩器 (gzip, deflate, zstd)
	options              *CompressOptions
	chosenEncoding       string // 最终选择的编码
	wroteHeader          bool
	doCompression        bool
	statusCode           int
}

var compressResponseWriterPool = sync.Pool{
	New: func() interface{} { return &compressResponseWriter{} },
}

func acquireCompressResponseWriter(underlying touka.ResponseWriter, opts *CompressOptions) *compressResponseWriter {
	crw := compressResponseWriterPool.Get().(*compressResponseWriter)
	crw.ResponseWriter = underlying
	crw.options = opts
	crw.chosenEncoding = ""
	crw.wroteHeader = false
	crw.doCompression = false
	crw.statusCode = 0
	crw.compressor = nil // 确保 compressor 被重置
	return crw
}

func releaseCompressResponseWriter(crw *compressResponseWriter) {
	if crw.compressor != nil {
		cfg, exists := crw.options.Algorithms[crw.chosenEncoding]
		poolEnabled := exists && cfg.PoolEnabled // 如果算法配置存在且启用了池

		_ = crw.compressor.Close()
		putCompressor(crw.compressor, crw.chosenEncoding, poolEnabled)
		crw.compressor = nil
	}
	crw.ResponseWriter = nil
	crw.options = nil
	compressResponseWriterPool.Put(crw)
}

// --- compressResponseWriter 方法实现 ---
func (crw *compressResponseWriter) Header() http.Header { return crw.ResponseWriter.Header() }

func (crw *compressResponseWriter) WriteHeader(statusCode int) {
	if crw.wroteHeader {
		return
	}
	crw.wroteHeader = true
	crw.statusCode = statusCode

	// 如果已决定不压缩 (例如，在 negotiateEncoding 中决定) 或者一些特定状态码，则直接写入
	if !crw.doCompression || statusCode < http.StatusOK || statusCode == http.StatusNoContent || statusCode == http.StatusResetContent || statusCode == http.StatusNotModified {
		crw.ResponseWriter.WriteHeader(statusCode)
		return
	}
	// 如果响应已被其他方式编码
	if crw.Header().Get(headerContentEncoding) != "" {
		crw.doCompression = false // 修正：确保标记为不压缩
		crw.ResponseWriter.WriteHeader(statusCode)
		return
	}

	// 检查 Content-Type 是否可压缩
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(crw.Header().Get(headerContentType), ";")[0]))
	compressibleTypes := crw.options.CompressibleTypes
	if len(compressibleTypes) == 0 {
		compressibleTypes = defaultCompressibleTypes
	}
	isCompressible := false
	for _, t := range compressibleTypes {
		if strings.HasPrefix(contentType, t) {
			isCompressible = true
			break
		}
	}
	if !isCompressible {
		crw.doCompression = false // 标记为不压缩
		crw.ResponseWriter.WriteHeader(statusCode)
		return
	}

	// 检查最小内容长度
	if crw.options.MinContentLength > 0 {
		if clStr := crw.Header().Get(headerContentLength); clStr != "" {
			if cl, err := strconv.ParseInt(clStr, 10, 64); err == nil && cl < crw.options.MinContentLength {
				crw.doCompression = false // 标记为不压缩
				crw.ResponseWriter.WriteHeader(statusCode)
				return
			}
		}
	}

	// 如果到这里，doCompression 仍然为 true，并且 chosenEncoding 应该已经被设置
	if !crw.doCompression || crw.chosenEncoding == "" || crw.chosenEncoding == encodingIdentity {
		crw.doCompression = false // 双重检查或处理 identity 的情况
		crw.ResponseWriter.WriteHeader(statusCode)
		return
	}

	// 所有检查通过，确认进行压缩
	crw.Header().Set(headerContentEncoding, crw.chosenEncoding)
	crw.Header().Add(headerVary, headerAcceptEncoding)
	crw.Header().Del(headerContentLength) // 压缩会改变内容长度

	algoConfig, ok := crw.options.Algorithms[crw.chosenEncoding]
	if !ok { // 如果 chosenEncoding 不在配置中，使用默认级别
		switch crw.chosenEncoding {
		case encodingGzip:
			algoConfig = AlgorithmConfig{Level: gzip.DefaultCompression, PoolEnabled: true}
		case encodingDeflate:
			algoConfig = AlgorithmConfig{Level: flate.DefaultCompression, PoolEnabled: true}
		case encodingZstd:
			algoConfig = AlgorithmConfig{Level: int(zstd.SpeedDefault), PoolEnabled: true} // zstd.SpeedDefault是3
		default: // 不应该发生
			crw.doCompression = false
			crw.ResponseWriter.WriteHeader(statusCode)
			return
		}
	}

	crw.compressor = getCompressor(crw.chosenEncoding, algoConfig.Level, crw.ResponseWriter, algoConfig.PoolEnabled)
	if crw.compressor == nil { // 获取压缩器失败
		crw.doCompression = false
		crw.Header().Del(headerContentEncoding) // 移除之前设置的编码头
		crw.Header().Del(headerVary)            // 也移除 Vary
		crw.ResponseWriter.WriteHeader(statusCode)
		return
	}

	crw.ResponseWriter.WriteHeader(statusCode) // 写入实际的状态码
}

func (crw *compressResponseWriter) Write(data []byte) (int, error) {
	if !crw.wroteHeader {
		crw.WriteHeader(http.StatusOK) // 隐式写入200 OK
	}
	if crw.doCompression && crw.compressor != nil {
		return crw.compressor.Write(data)
	}
	return crw.ResponseWriter.Write(data)
}

func (crw *compressResponseWriter) Close() error { // 主要供 defer 调用
	if crw.compressor != nil {
		// Close 应该由 releaseCompressResponseWriter 处理，这里仅作为防御
		// err := crw.compressor.Close()
		// putCompressor(crw.compressor, crw.chosenEncoding, crw.options.Algorithms[crw.chosenEncoding].PoolEnabled)
		// crw.compressor = nil
		// return err
	}
	return nil
}

func (crw *compressResponseWriter) Flush() {
	if crw.doCompression && crw.compressor != nil {
		_ = crw.compressor.Flush() // 忽略刷新错误，或记录
	}
	if fl, ok := crw.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (crw *compressResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := crw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("touka.compressResponseWriter: underlying ResponseWriter does not implement http.Hijacker") // 英文错误
}
func (crw *compressResponseWriter) Status() int {
	if crw.statusCode != 0 {
		return crw.statusCode
	}
	return crw.ResponseWriter.Status()
}
func (crw *compressResponseWriter) Size() int     { return crw.ResponseWriter.Size() }
func (crw *compressResponseWriter) Written() bool { return crw.ResponseWriter.Written() }

// --- 压缩中间件 ---

// Compression 返回一个通用的压缩中间件，支持 Gzip, Deflate, Zstd。
// 它会根据客户端的 Accept-Encoding 头部和服务器配置选择最佳的压缩算法。
func Compression(opts CompressOptions) touka.HandlerFunc {
	if opts.Algorithms == nil && len(opts.CompressibleTypes) == 0 && len(opts.EncodingPriority) == 0 && opts.MinContentLength == 0 {
		opts = DefaultCompressionConfig()
	}

	// 设置默认算法配置 (如果用户没有提供)
	if opts.Algorithms == nil {
		opts.Algorithms = make(map[string]AlgorithmConfig)
	}
	if _, ok := opts.Algorithms[encodingGzip]; !ok {
		opts.Algorithms[encodingGzip] = AlgorithmConfig{Level: gzip.DefaultCompression, PoolEnabled: true}
	}
	if _, ok := opts.Algorithms[encodingDeflate]; !ok {
		opts.Algorithms[encodingDeflate] = AlgorithmConfig{Level: flate.DefaultCompression, PoolEnabled: true}
	}
	// Zstd 默认不启用，除非用户在 opts.Algorithms 中明确配置
	// 例如：opts.Algorithms[encodingZstd] = AlgorithmConfig{Level: int(zstd.SpeedDefault), PoolEnabled: true}

	// 设置默认编码优先级
	if len(opts.EncodingPriority) == 0 {
		defaultPrio := []string{}
		if _, ok := opts.Algorithms[encodingZstd]; ok {
			defaultPrio = append(defaultPrio, encodingZstd)
		}
		if _, ok := opts.Algorithms[encodingGzip]; ok {
			defaultPrio = append(defaultPrio, encodingGzip)
		}
		if _, ok := opts.Algorithms[encodingDeflate]; ok {
			defaultPrio = append(defaultPrio, encodingDeflate)
		}
		opts.EncodingPriority = defaultPrio
	}

	return func(c *touka.Context) {
		// 1. 解析 Accept-Encoding 头部
		clientAcceptedEncodings := parseAcceptEncoding(c.Request.Header.Get(headerAcceptEncoding))

		// 2. 协商选择编码
		chosenEncoding := negotiateEncoding(clientAcceptedEncodings, opts.Algorithms, opts.EncodingPriority)

		// 如果选择不压缩 (identity) 或者没有可用的压缩算法，则直接进入下一个处理器
		if chosenEncoding == "" || chosenEncoding == encodingIdentity {
			c.Next()
			return
		}

		// 3. 包装 ResponseWriter
		originalWriter := c.Writer
		crw := acquireCompressResponseWriter(originalWriter, &opts)
		crw.chosenEncoding = chosenEncoding // 告诉 writer 我们选择了什么编码
		crw.doCompression = true            // 初步标记为需要压缩，WriteHeader 会做最终检查

		c.Writer = crw // 替换上下文的 writer

		defer func() {
			// 关闭压缩器（如果已创建）并将其返回到池中，然后恢复原始 writer
			// releaseCompressResponseWriter 会处理 crw.compressor.Close()
			releaseCompressResponseWriter(crw)
			c.Writer = originalWriter
		}()

		// 4. 调用链中的下一个处理函数
		c.Next()

		// c.Next() 返回后，如果 crw.doCompression 仍然为 true，
		// 且 crw.compressor 已创建，则响应已被写入压缩器。
		// defer 中的 releaseCompressResponseWriter 会负责关闭压缩器并刷新剩余数据。
	}
}

// DefaultCompressionConfig 获取默认配置
func DefaultCompressionConfig() CompressOptions {
	return CompressOptions{
		Algorithms: map[string]AlgorithmConfig{
			encodingGzip:    {Level: gzip.DefaultCompression, PoolEnabled: true},
			encodingDeflate: {Level: flate.DefaultCompression, PoolEnabled: true},
		},
		MinContentLength:  0,
		CompressibleTypes: defaultCompressibleTypes,
		EncodingPriority:  []string{encodingGzip, encodingDeflate},
	}
}

// qValue 代表 Accept-Encoding 中的 q 值
type qValue struct {
	value string
	q     float64
}

// parseAcceptEncoding 解析 Accept-Encoding 头部字符串
func parseAcceptEncoding(header string) []qValue {
	if header == "" {
		return nil
	}

	var qValues []qValue
	parts := strings.Split(header, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		params := strings.Split(part, ";")
		val := strings.TrimSpace(params[0])
		q := 1.0 // 默认 q=1.0

		if len(params) > 1 {
			for _, p := range params[1:] {
				p = strings.TrimSpace(p)
				if strings.HasPrefix(p, "q=") {
					if qVal, err := strconv.ParseFloat(strings.TrimPrefix(p, "q="), 64); err == nil {
						if qVal >= 0 && qVal <= 1 { // q 值必须在 0 到 1 之间
							q = qVal
						} else if qVal > 1 {
							q = 1.0 // RFC 规定大于1视为1
						} else {
							q = 0 // 小于0视为0 (不接受)
						}
						break
					} else {
						q = 0 // 解析失败，视为不接受
						break
					}
				}
			}
		}
		if q > 0 { // 只添加客户端接受的编码 (q > 0)
			qValues = append(qValues, qValue{value: val, q: q})
		}
	}

	// 根据 q 值降序排序，如果 q 值相同，则按原始顺序（通常不重要）
	sort.SliceStable(qValues, func(i, j int) bool {
		return qValues[i].q > qValues[j].q
	})

	return qValues
}

// negotiateEncoding 根据客户端接受的编码、服务器支持的算法和优先级选择最佳编码
func negotiateEncoding(clientPrefs []qValue, serverAlgos map[string]AlgorithmConfig, serverPrio []string) string {
	if len(clientPrefs) == 0 { // 如果客户端没有指定 Accept-Encoding，有些服务器可能会默认 gzip
		// RFC 7231 Section 5.3.4: "If no Accept-Encoding field is present in a request,
		// the server MAY compress the response with any content coding it deems appropriate."
		// 然而，更安全的做法是，如果没有明确的 Accept-Encoding，则不压缩或仅压缩 gzip (如果服务器配置了)。
		// 为了简单和明确，如果客户端没说，我们就不压缩。
		return encodingIdentity // 或者 "" 表示不选择任何特定编码
	}

	// 检查客户端是否明确接受 identity (不压缩)
	clientAcceptsIdentity := false
	identityQValue := 0.0
	acceptsAnything := false // 是否有 *

	for _, pref := range clientPrefs {
		if pref.value == encodingIdentity {
			clientAcceptsIdentity = true
			identityQValue = pref.q
			// q=0 的 identity 表示不接受 identity，但我们已在 parseAcceptEncoding 中过滤了 q=0
		}
		if pref.value == "*" {
			acceptsAnything = true
			// 如果 * 的 q 值大于 identity 的 q 值，那么我们可能还是会选择压缩
			// 通常 * 的 q 值较低
		}
	}

	// 遍历服务器配置的优先级列表
	for _, serverAlgoName := range serverPrio {
		if _, supportedByServer := serverAlgos[serverAlgoName]; !supportedByServer {
			continue // 服务器未配置此算法
		}
		// 检查客户端是否接受此算法
		for _, clientPref := range clientPrefs {
			if clientPref.value == serverAlgoName && clientPref.q > 0 {
				// 客户端接受此算法，且 q > 0
				// 如果客户端也接受 identity，我们需要比较 q 值
				// 通常压缩算法的 q 值会是 1.0，identity 可能会是 0.5 或未指定 (也是1.0)
				// 规则：如果 identity 的 q 值非常低，我们倾向于压缩。
				// 如果 identity 的 q 值很高，并且高于我们选的压缩算法，则应选 identity。
				// 简单起见：如果选中的压缩算法的 q 值高于 identity 的 q 值，则使用它。
				// 或者如果 identity q=0 (我们之前过滤了，但如果不过滤，这里要处理)
				if clientAcceptsIdentity && identityQValue >= clientPref.q && identityQValue > 0 {
					// 如果 identity 更受偏好或同等偏好，并且 identity 可接受，则不压缩。
					// 但通常我们希望尽可能压缩，除非 identity 被明确高度偏好。
					// 一个更常见的策略是：只要客户端接受某个压缩算法 (q>0)，就使用它，
					// 除非 identity;q=0 (表示不接受 identity，但这是不可能的，因为 q=0 的 identity 表示客户端不接受不压缩的内容，这本身就有问题)
					// 或 identity;q=1 且其他所有算法 q<1。
					// 简化：如果 identity 的 q 值不为0，并且比当前算法的 q 值高，则可能不压缩。
					// 但一般只要客户端接受压缩，我们就压缩。
					// 只有当 identity 的 q 值最高，并且没有其他 q 值更高的压缩算法时，才不压缩。
					// 这里我们假设：只要客户端接受一种压缩算法，且服务器也支持，就优先选择压缩。
				}
				return serverAlgoName // 找到第一个客户端接受且服务器支持的算法
			}
		}
	}

	// 如果客户端接受 * (任何编码)，并且服务器有配置的算法在此之前未被匹配
	if acceptsAnything {
		for _, serverAlgoName := range serverPrio { // 再次遍历服务器优先级
			if _, supportedByServer := serverAlgos[serverAlgoName]; supportedByServer {
				// 检查这个 serverAlgoName 是否在 clientPrefs 中被显式拒绝 (q=0)
				// (我们的 parseAcceptEncoding 已经处理了 q=0 的情况，不会包含它们)
				return serverAlgoName // 选择服务器支持的第一个作为通配符匹配
			}
		}
	}

	// 如果遍历完所有服务器优先的算法，客户端都不接受，
	// 或者客户端只接受 identity (q>0)，那么返回 identity
	if clientAcceptsIdentity && identityQValue > 0 {
		return encodingIdentity
	}

	return "" // 没有可接受的编码，或只接受 q=0 的编码 (不应发生)
}
