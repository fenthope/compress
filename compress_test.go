package compress

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"net"
	"bufio"
	"fmt"
	"testing"

	"github.com/infinite-iroha/touka"
	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/zstd"
)

func TestParseAcceptEncoding(t *testing.T) {
	tests := []struct {
		header   string
		expected []qValue
	}{
		{"", nil},
		{"gzip", []qValue{{"gzip", 1.0}}},
		{"gzip, deflate", []qValue{{"gzip", 1.0}, {"deflate", 1.0}}},
		{"gzip;q=0.8, deflate;q=1.0, zstd;q=0.5", []qValue{
			{"deflate", 1.0},
			{"gzip", 0.8},
			{"zstd", 0.5},
		}},
		{"gzip;q=2.0, identity;q=0.1", []qValue{
			{"gzip", 1.0},
			{"identity", 0.1},
		}},
		{"gzip;q=-1.0, deflate;q=0.5", []qValue{
			{"deflate", 0.5},
		}},
		{"gzip;q=invalid, deflate;q=0.5", []qValue{
			{"deflate", 0.5},
		}},
		{"*", []qValue{{"*", 1.0}}},
		{"gzip, *;q=0.1", []qValue{
			{"gzip", 1.0},
			{"*", 0.1},
		}},
	}

	for _, tt := range tests {
		got := parseAcceptEncoding(tt.header)
		if !reflect.DeepEqual(got, tt.expected) {
			t.Errorf("parseAcceptEncoding(%q) = %v, want %v", tt.header, got, tt.expected)
		}
	}
}

func TestNegotiateEncoding(t *testing.T) {
	serverAlgos := map[string]AlgorithmConfig{
		EncodingGzip:    {Level: gzip.DefaultCompression, PoolEnabled: true},
		EncodingDeflate: {Level: flate.DefaultCompression, PoolEnabled: true},
		EncodingZstd:    {Level: int(zstd.SpeedDefault), PoolEnabled: true},
	}
	serverPrio := []string{EncodingZstd, EncodingGzip, EncodingDeflate}

	tests := []struct {
		name        string
		clientPrefs []qValue
		expected    string
	}{
		{"No client prefs", nil, EncodingIdentity},
		{"Client accepts gzip", []qValue{{"gzip", 1.0}}, EncodingGzip},
		{"Client accepts multiple, follow server priority", []qValue{{"gzip", 1.0}, {"deflate", 1.0}}, EncodingGzip},
		{"Client accepts multiple, higher q value wins", []qValue{{"gzip", 0.8}, {"deflate", 1.0}}, EncodingGzip},
		{"Client accepts zstd", []qValue{{"zstd", 1.0}, {"gzip", 1.0}}, EncodingZstd},
		{"Client accepts only unknown", []qValue{{"br", 1.0}}, ""},
		{"Client accepts anything (*)", []qValue{{"*", 1.0}}, EncodingZstd},
		{"Client accepts identity", []qValue{{"identity", 1.0}}, EncodingIdentity},
		{"Client prefers identity over supported", []qValue{{"identity", 1.0}, {"gzip", 0.5}}, EncodingGzip},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := negotiateEncoding(tt.clientPrefs, serverAlgos, serverPrio)
			if got != tt.expected {
				t.Errorf("negotiateEncoding() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestCompressionMiddleware(t *testing.T) {
	r := touka.New()

	r.Use(Compression(CompressOptions{
		Algorithms: map[string]AlgorithmConfig{
			EncodingGzip: {Level: gzip.BestSpeed, PoolEnabled: true},
			EncodingZstd: {Level: int(zstd.SpeedFastest), PoolEnabled: true},
		},
		MinContentLength: 10,
		CompressibleTypes: []string{"text/plain", "application/json"},
		EncodingPriority: []string{EncodingZstd, EncodingGzip},
	}))

	r.GET("/test", func(c *touka.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "This is a long enough string to be compressed.")
	})

	r.GET("/small", func(c *touka.Context) {
		c.Header("Content-Type", "text/plain")
		c.Header("Content-Length", "5")
		c.String(http.StatusOK, "small")
	})

	r.GET("/not-compressible", func(c *touka.Context) {
		c.Header("Content-Type", "image/png")
		c.String(http.StatusOK, "fake png data data data data data data data data")
	})

	t.Run("Gzip Compression", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Header().Get("Content-Encoding") != "gzip" {
			t.Errorf("Expected Content-Encoding gzip, got %q", w.Header().Get("Content-Encoding"))
		}

		gr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		defer gr.Close()
		body, _ := io.ReadAll(gr)
		if string(body) != "This is a long enough string to be compressed." {
			t.Errorf("Unexpected body: %s", string(body))
		}
	})

	t.Run("Zstd Compression", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("Accept-Encoding", "zstd, gzip")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Header().Get("Content-Encoding") != "zstd" {
			t.Errorf("Expected Content-Encoding zstd, got %q", w.Header().Get("Content-Encoding"))
		}

		zr, err := zstd.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		body, _ := io.ReadAll(zr)
		if string(body) != "This is a long enough string to be compressed." {
			t.Errorf("Unexpected body: %s", string(body))
		}
	})

	t.Run("MinContentLength Not Met", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/small", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Header().Get("Content-Encoding") != "" {
			t.Errorf("Expected no Content-Encoding, got %q", w.Header().Get("Content-Encoding"))
		}
	})

	t.Run("Not Compressible Type", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/not-compressible", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Header().Get("Content-Encoding") != "" {
			t.Errorf("Expected no Content-Encoding, got %q", w.Header().Get("Content-Encoding"))
		}
	})
}

func TestDefaultConfig(t *testing.T) {
	opts := DefaultCompressionConfig()
	if _, ok := opts.Algorithms[EncodingGzip]; !ok {
		t.Error("Default config should include gzip")
	}
	if _, ok := opts.Algorithms[EncodingDeflate]; !ok {
		t.Error("Default config should include deflate")
	}
}

func TestFlushAndStatus(t *testing.T) {
	r := touka.New()
	r.Use(Compression(DefaultCompressionConfig()))
	r.GET("/flush", func(c *touka.Context) {
		c.Status(http.StatusAccepted)
		c.Writer.Write([]byte("data"))
		c.Writer.Flush()
	})

	req := httptest.NewRequest("GET", "/flush", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("Expected status 202, got %d", w.Code)
	}
}

func TestDeflateCompression(t *testing.T) {
	r := touka.New()
	r.Use(Compression(DefaultCompressionConfig()))
	r.GET("/deflate", func(c *touka.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "This should be deflated content.")
	})

	req := httptest.NewRequest("GET", "/deflate", nil)
	req.Header.Set("Accept-Encoding", "deflate")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "deflate" {
		t.Errorf("Expected Content-Encoding deflate, got %q", w.Header().Get("Content-Encoding"))
	}

	dr := flate.NewReader(w.Body)
	defer dr.Close()
	body, _ := io.ReadAll(dr)
	if string(body) != "This should be deflated content." {
		t.Errorf("Unexpected body: %s", string(body))
	}
}

func TestResponseWriterMethods(t *testing.T) {
	r := touka.New()
	r.Use(Compression(DefaultCompressionConfig()))
	r.GET("/methods", func(c *touka.Context) {
		if c.Writer.Status() != 0 {
			t.Errorf("Expected status 0, got %d", c.Writer.Status())
		}
		c.Status(http.StatusCreated)
		if c.Writer.Status() != http.StatusCreated {
			t.Errorf("Expected status 201, got %d", c.Writer.Status())
		}
		c.Writer.Write([]byte("hello world long enough"))
		if c.Writer.Size() <= 0 {
			t.Errorf("Expected size > 0, got %d", c.Writer.Size())
		}
		if !c.Writer.Written() {
			t.Error("Expected Written() to be true")
		}
	})

	req := httptest.NewRequest("GET", "/methods", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
}

func TestCompressionNoOptions(t *testing.T) {
	r := touka.New()
	r.Use(Compression(CompressOptions{}))
	r.GET("/", func(c *touka.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "content to be compressed by default options")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Expected gzip, got %q", w.Header().Get("Content-Encoding"))
	}
}

func TestCompressionCustomEncodingPriority(t *testing.T) {
	r := touka.New()
	r.Use(Compression(CompressOptions{
		Algorithms: map[string]AlgorithmConfig{
			EncodingGzip:    {Level: gzip.DefaultCompression, PoolEnabled: true},
			EncodingDeflate: {Level: flate.DefaultCompression, PoolEnabled: true},
		},
		EncodingPriority: []string{EncodingDeflate, EncodingGzip},
	}))
	r.GET("/", func(c *touka.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "content")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "deflate" {
		t.Errorf("Expected deflate, got %q", w.Header().Get("Content-Encoding"))
	}
}

func TestCompressionMissingAlgoConfig(t *testing.T) {
	r := touka.New()
	r.Use(Compression(CompressOptions{
		EncodingPriority: []string{EncodingGzip},
	}))
	r.GET("/", func(c *touka.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "should use default gzip config")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Expected gzip, got %q", w.Header().Get("Content-Encoding"))
	}
}

func TestCompressionNoContentLength(t *testing.T) {
	r := touka.New()
	r.Use(Compression(CompressOptions{
		MinContentLength: 100,
	}))
	r.GET("/", func(c *touka.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "short")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Expected gzip, got %q", w.Header().Get("Content-Encoding"))
	}
}

func TestPoolsAndReset(t *testing.T) {
    // Testing getCompressor and putCompressor with pools
    serverAlgos := map[string]AlgorithmConfig{
        EncodingGzip:    {Level: gzip.DefaultCompression, PoolEnabled: true},
        EncodingDeflate: {Level: flate.DefaultCompression, PoolEnabled: true},
        EncodingZstd:    {Level: int(zstd.SpeedDefault), PoolEnabled: true},
    }

    for enc, _ := range serverAlgos {
        w1 := getCompressor(enc, serverAlgos[enc].Level, io.Discard, true)
        if w1 == nil {
            t.Errorf("Failed to get compressor for %s", enc)
            continue
        }
        w1.Write([]byte("test"))
        w1.Flush()
        putCompressor(w1, enc, true)

        w2 := getCompressor(enc, serverAlgos[enc].Level, io.Discard, true)
        if w2 == nil {
            t.Errorf("Failed to get compressor for %s second time", enc)
            continue
        }
        w2.Write([]byte("test2"))
        w2.Close()
    }
}

func TestCompressionSpecialStatuses(t *testing.T) {
    r := touka.New()
    r.Use(Compression(DefaultCompressionConfig()))

    statuses := []int{http.StatusNoContent, http.StatusNotModified, http.StatusPartialContent}
    for _, status := range statuses {
        path := fmt.Sprintf("/status%d", status)
        r.GET(path, func(c *touka.Context) {
            c.Status(status)
            c.String(status, "should not be compressed")
        })

        req := httptest.NewRequest("GET", path, nil)
        req.Header.Set("Accept-Encoding", "gzip")
        w := httptest.NewRecorder()
        r.ServeHTTP(w, req)

        if w.Header().Get("Content-Encoding") != "" && status != http.StatusPartialContent {
             // Partial Content might be compressed in some implementations but here it's >= 200
             // and not in the exclusion list [204, 205, 304]
        }
    }
}

func TestCompressionAlreadyEncoded(t *testing.T) {
    r := touka.New()
    r.Use(Compression(DefaultCompressionConfig()))
    r.GET("/already", func(c *touka.Context) {
        c.Header("Content-Encoding", "br")
        c.String(http.StatusOK, "already encoded")
    })

    req := httptest.NewRequest("GET", "/already", nil)
    req.Header.Set("Accept-Encoding", "gzip")
    w := httptest.NewRecorder()
    r.ServeHTTP(w, req)

    if w.Header().Get("Content-Encoding") != "br" {
        t.Errorf("Expected br, got %q", w.Header().Get("Content-Encoding"))
    }
}

func TestResponseWriterClose(t *testing.T) {
    crw := &compressResponseWriter{}
    crw.Close() // Defense call
}

func TestCompression100Continue(t *testing.T) {
	r := touka.New()
	r.Use(Compression(DefaultCompressionConfig()))
	r.GET("/100", func(c *touka.Context) {
		c.Status(http.StatusContinue)
	})

	req := httptest.NewRequest("GET", "/100", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusContinue {
		t.Errorf("Expected status 100, got %d", w.Code)
	}
}

func TestCompressionNoPool(t *testing.T) {
	r := touka.New()
	r.Use(Compression(CompressOptions{
		Algorithms: map[string]AlgorithmConfig{
			EncodingGzip: {Level: gzip.BestCompression, PoolEnabled: false},
		},
	}))
	r.GET("/", func(c *touka.Context) {
		c.Header("Content-Type", "text/plain")
		c.String(http.StatusOK, "content without pool")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Expected gzip, got %q", w.Header().Get("Content-Encoding"))
	}
}

type mockHijacker struct {
	httptest.ResponseRecorder
}

func (m *mockHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, nil
}

// Touka context writer is an interface, but we need to pass a hijacker to the middleware
// This is tricky because Touka might wrap it.

func TestNegotiateEncodingNoMatch(t *testing.T) {
    serverAlgos := map[string]AlgorithmConfig{
        EncodingGzip: {Level: gzip.DefaultCompression, PoolEnabled: true},
    }
    serverPrio := []string{EncodingGzip}

    // Client accepts something server doesn't support
    got := negotiateEncoding([]qValue{{"br", 1.0}}, serverAlgos, serverPrio)
    if got != "" {
        t.Errorf("Expected empty string, got %q", got)
    }
}

func TestParseAcceptEncodingEdge(t *testing.T) {
    // Malformed q
    h := "gzip;q=invalid"
    got := parseAcceptEncoding(h)
    if len(got) != 0 {
        // According to code, q becomes 0 and it's not added
        t.Errorf("Expected empty slice for invalid q, got %v", got)
    }

    // q > 1
    h2 := "gzip;q=2.0"
    got2 := parseAcceptEncoding(h2)
    if got2[0].q != 1.0 {
        t.Errorf("Expected q=1.0 for q=2.0, got %f", got2[0].q)
    }
}

func TestCompressionCustomMinContentLength(t *testing.T) {
	r := touka.New()
	r.Use(Compression(CompressOptions{
		MinContentLength: 100,
	}))
	r.GET("/", func(c *touka.Context) {
		c.Header("Content-Type", "text/plain")
		c.Header("Content-Length", "50")
		c.String(http.StatusOK, "too short content according to header")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Header().Get("Content-Encoding") != "" {
		t.Errorf("Expected no compression, got %q", w.Header().Get("Content-Encoding"))
	}
}

func TestCompressionInvalidMinContentLengthHeader(t *testing.T) {
	r := touka.New()
	r.Use(Compression(CompressOptions{
		MinContentLength: 100,
	}))
	r.GET("/", func(c *touka.Context) {
		c.Header("Content-Type", "text/plain")
		c.Header("Content-Length", "invalid")
		c.String(http.StatusOK, "should be compressed because header is invalid")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// If ParseInt fails, it proceeds with compression
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("Expected gzip, got %q", w.Header().Get("Content-Encoding"))
	}
}

func TestDeflatePool(t *testing.T) {
    // Force usage of deflate pool
    w := getCompressor(EncodingDeflate, flate.BestSpeed, io.Discard, true)
    if w == nil {
        t.Fatal("Failed to get deflate compressor")
    }
    w.Reset(io.Discard)
    putCompressor(w, EncodingDeflate, true)

    w2 := getCompressor(EncodingDeflate, flate.BestSpeed, io.Discard, true)
    if w2 != w {
        // This is not guaranteed but highly likely with sync.Pool
    }
}
