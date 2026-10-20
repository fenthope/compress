package compress

import (
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/infinite-iroha/touka"
	"github.com/klauspost/compress/zstd"
)

// 池槽位必须覆盖所有支持的压缩级别。
// 历史 bug：DefaultCompression/NoCompression 的槽位从未初始化（nil），
// get/put 直接绕过池，导致每个响应都新建一个约 1MB 的编码器。
func TestPoolSlotsCoverAllLevels(t *testing.T) {
	for _, level := range gzipLevels {
		idx := slotIndex(gzipLevels, level)
		if idx < 0 {
			t.Fatalf("gzip 级别 %d 不在槽位表中", level)
		}
		if gzipWriterPools[idx] == nil {
			t.Errorf("gzip 级别 %d 的池为 nil", level)
		}
	}
	for _, level := range deflateLevels {
		idx := slotIndex(deflateLevels, level)
		if idx < 0 {
			t.Fatalf("deflate 级别 %d 不在槽位表中", level)
		}
		if deflateWriterPools[idx] == nil {
			t.Errorf("deflate 级别 %d 的池为 nil", level)
		}
	}
	for _, level := range zstdLevels {
		if zstdWriterPools[level] == nil {
			t.Errorf("zstd 级别 %v 的池为 nil", level)
		}
	}
}

// 连续取还时应当反复拿到同一个编码器（池真的被用上了）。
// 用复用率而不是「指针必须相同」来断言：sync.Pool 的内容会在 GC 时被清空，
// -race 下 race detector 也会强制 GC，单次指针比较会随机失败。
// 修复前每一次都是新建编码器，复用率为 0。
func TestPoolReusesCompressor(t *testing.T) {
	const rounds = 50

	assertReuse := func(name string, enc string, level int, get func() compressWriter) {
		var prev compressWriter
		reused := 0
		for i := 0; i < rounds; i++ {
			w := get()
			if w == nil {
				t.Fatalf("%s 级别 %v 取不到压缩器", name, level)
			}
			_, _ = w.Write([]byte("hello"))
			_ = w.Close()
			putCompressor(w, enc, true)
			if prev != nil && prev == w {
				reused++
			}
			prev = w
		}
		if reused*2 < rounds {
			t.Errorf("%s 级别 %v 复用率过低：%d/%d 次拿到了同一个编码器", name, level, reused, rounds)
		}
	}

	for _, level := range []int{gzip.HuffmanOnly, gzip.DefaultCompression, gzip.NoCompression, gzip.BestSpeed, gzip.BestCompression} {
		level := level
		assertReuse("gzip", EncodingGzip, level, func() compressWriter {
			return getCompressor(EncodingGzip, level, io.Discard, true)
		})
	}
	for _, level := range []int{flate.HuffmanOnly, flate.DefaultCompression, flate.NoCompression, flate.BestSpeed, flate.BestCompression} {
		level := level
		assertReuse("deflate", EncodingDeflate, level, func() compressWriter {
			return getCompressor(EncodingDeflate, level, io.Discard, true)
		})
	}
	for _, level := range zstdLevels {
		level := level
		assertReuse("zstd", EncodingZstd, int(level), func() compressWriter {
			return getCompressor(EncodingZstd, int(level), io.Discard, true)
		})
	}
}

// readerCloser 把各算法的解码器统一成 io.ReadCloser（zstd.Decoder.Close 无返回值）。
type readerCloser struct {
	io.Reader
	close func()
}

func (rc readerCloser) Close() error {
	rc.close()
	return nil
}

// 复用后的编码器必须仍然产出正确内容（池化不能破坏压缩结果）。
func TestPoolReuseRoundTrip(t *testing.T) {
	const payload = `{"ok":true,"id":"L000001","data":"0123456789abcdef"}`
	cases := []struct {
		encoding string
		level    int
		decode   func(io.Reader) (io.ReadCloser, error)
	}{
		{EncodingGzip, gzip.DefaultCompression, func(r io.Reader) (io.ReadCloser, error) {
			gr, err := gzip.NewReader(r)
			return readerCloser{gr, func() { _ = gr.Close() }}, err
		}},
		{EncodingGzip, gzip.BestSpeed, func(r io.Reader) (io.ReadCloser, error) {
			gr, err := gzip.NewReader(r)
			return readerCloser{gr, func() { _ = gr.Close() }}, err
		}},
		{EncodingDeflate, flate.DefaultCompression, func(r io.Reader) (io.ReadCloser, error) {
			fr := flate.NewReader(r)
			return readerCloser{fr, func() { _ = fr.Close() }}, nil
		}},
		{EncodingDeflate, flate.BestSpeed, func(r io.Reader) (io.ReadCloser, error) {
			fr := flate.NewReader(r)
			return readerCloser{fr, func() { _ = fr.Close() }}, nil
		}},
		{EncodingZstd, int(zstd.SpeedDefault), func(r io.Reader) (io.ReadCloser, error) {
			zr, err := zstd.NewReader(r)
			return readerCloser{zr, func() { zr.Close() }}, err
		}},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%d", tc.encoding, tc.level), func(t *testing.T) {
			r := touka.New()
			r.Use(Compression(CompressOptions{
				Algorithms: map[string]AlgorithmConfig{
					tc.encoding: {Level: tc.level, PoolEnabled: true},
				},
				EncodingPriority: []string{tc.encoding},
			}))
			r.GET("/x", func(c *touka.Context) {
				c.Header("Content-Type", "application/json")
				c.String(http.StatusOK, payload)
			})
			req := httptest.NewRequest("GET", "/x", nil)
			req.Header.Set("Accept-Encoding", tc.encoding)
			// 多次请求，覆盖池命中与新建两种路径
			for i := 0; i < 5; i++ {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if got := w.Header().Get("Content-Encoding"); got != tc.encoding {
					t.Fatalf("期望 Content-Encoding %s，实际 %q", tc.encoding, got)
				}
				dec, err := tc.decode(w.Body)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(dec)
				if err != nil {
					t.Fatal(err)
				}
				_ = dec.Close()
				if string(body) != payload {
					t.Fatalf("第 %d 次响应内容错误：%q", i, string(body))
				}
			}
		})
	}
}

// 每个响应不应该再新建一个约 1MB 的编码器（修复前 DefaultCompression 是 1053KB/请求）。
// 测量期间关闭 GC：sync.Pool 的内容会在 GC 时被清空，否则统计值取决于 GC 时机。
func TestNoEncoderAllocPerRequest(t *testing.T) {
	r := touka.New()
	r.Use(Compression(DefaultCompressionConfig()))
	r.GET("/x", func(c *touka.Context) {
		c.Header("Content-Type", "application/json")
		c.String(http.StatusOK, `{"ok":true,"id":"L000001"}`)
	})
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	for i := 0; i < 20; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}
	if raceEnabled {
		t.Skip("race 模式会触发 GC，sync.Pool 内容会被清空，分配量不可比")
	}
	oldGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(oldGC)
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	const n = 200
	for i := 0; i < n; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}
	runtime.ReadMemStats(&m1)
	perReq := float64(m1.TotalAlloc-m0.TotalAlloc) / n
	if perReq > 100*1024 {
		t.Errorf("每请求分配 %.1f KB，超过 100KB 阈值：压缩器可能被重复创建", perReq/1024)
	}
}

// 空闲编码器必须跨 GC 存活：sync.Pool 每次 GC 都会被清空，
// 压缩器被反复重建（gzip 编码器约 470KB/次）。
func TestPoolSurvivesGC(t *testing.T) {
	w1 := getCompressor(EncodingGzip, gzip.DefaultCompression, io.Discard, true)
	if w1 == nil {
		t.Fatal("取不到 gzip 压缩器")
	}
	_, _ = w1.Write([]byte("hello"))
	_ = w1.Close()
	putCompressor(w1, EncodingGzip, true)

	// 触发多次 GC，清空 sync.Pool 的 victim cache
	for i := 0; i < 3; i++ {
		runtime.GC()
	}

	w2 := getCompressor(EncodingGzip, gzip.DefaultCompression, io.Discard, true)
	if w2 == nil {
		t.Fatal("GC 之后取不到 gzip 压缩器")
	}
	if w1 != w2 {
		t.Error("GC 之后编码器没有被复用：空闲列表没有跨 GC 存活")
	}
	_, _ = w2.Write([]byte("world"))
	_ = w2.Close()
	putCompressor(w2, EncodingGzip, true)
}

// 空闲列表必须有上限：超出容量的编码器直接丢弃，不能无限堆积。
func TestPoolIsBounded(t *testing.T) {
	idx := slotIndex(gzipLevels, gzip.BestSpeed)
	pool := gzipWriterPools[idx]
	cap := cap(pool.ch)
	if cap <= 0 {
		t.Fatalf("空闲列表容量异常：%d", cap)
	}
	for i := 0; i < cap+3; i++ {
		w := getCompressor(EncodingGzip, gzip.BestSpeed, io.Discard, true)
		if w == nil {
			t.Fatal("取不到 gzip 压缩器")
		}
		putCompressor(w, EncodingGzip, true)
	}
	if got := len(pool.ch); got > cap {
		t.Errorf("空闲列表长度 %d 超过上限 %d", got, cap)
	}
}
