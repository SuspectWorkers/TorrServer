package log

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	DefaultMaxLogBytes int64 = 10 * 1024 * 1024 // 10 MB per log file (+ 1 rotated .1 backup)
	DefaultRingBytes         = 2 * 1024 * 1024  // 2 MB in-memory ring buffer
)

var (
	StartTime = time.Now()

	origStdout = os.Stdout
	origStderr = os.Stderr

	logPath    = ""
	webLogPath = ""

	logMu       sync.Mutex
	maxLogBytes = resolveMaxLogBytes()
	ringBuf     = newRingBuffer(DefaultRingBytes)
)

var webLog *log.Logger

var (
	logFile    *os.File
	webLogFile *os.File
)

func resolveMaxLogBytes() int64 {
	if raw := strings.TrimSpace(os.Getenv("TS_LOG_MAX_SIZE_MB")); raw != "" {
		if mb, err := strconv.ParseInt(raw, 10, 64); err == nil && mb > 0 {
			return mb * 1024 * 1024
		}
	}
	return DefaultMaxLogBytes
}

// SetMaxLogBytes overrides the maximum size per log file before rotation.
// Primarily intended for tests and custom embedding.
func SetMaxLogBytes(n int64) {
	logMu.Lock()
	if n > 0 {
		maxLogBytes = n
	}
	logMu.Unlock()
}

type ringBuffer struct {
	mu   sync.Mutex
	buf  []byte
	pos  int
	full bool
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{buf: make([]byte, capacity)}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	capLen := len(r.buf)
	if capLen == 0 {
		return len(p), nil
	}
	if len(p) >= capLen {
		copy(r.buf, p[len(p)-capLen:])
		r.pos = 0
		r.full = true
		return len(p), nil
	}

	first := capLen - r.pos
	if first >= len(p) {
		copy(r.buf[r.pos:], p)
		r.pos += len(p)
		if r.pos == capLen {
			r.pos = 0
			r.full = true
		}
		return len(p), nil
	}

	copy(r.buf[r.pos:], p[:first])
	rem := len(p) - first
	copy(r.buf[:rem], p[first:])
	r.pos = rem
	r.full = true
	return len(p), nil
}

func (r *ringBuffer) Bytes(maxBytes int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()

	var out []byte
	if !r.full {
		out = make([]byte, r.pos)
		copy(out, r.buf[:r.pos])
	} else {
		capLen := len(r.buf)
		out = make([]byte, capLen)
		n := copy(out, r.buf[r.pos:])
		copy(out[n:], r.buf[:r.pos])
		if idx := bytes.IndexByte(out, '\n'); idx >= 0 && idx+1 < len(out) {
			out = out[idx+1:]
		}
	}
	return trimTailLines(out, maxBytes)
}

func (r *ringBuffer) Reset() {
	r.mu.Lock()
	r.pos = 0
	r.full = false
	r.mu.Unlock()
}

func trimTailLines(data []byte, maxBytes int) []byte {
	if maxBytes > 0 && len(data) > maxBytes {
		data = data[len(data)-maxBytes:]
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 && idx+1 < len(data) {
			data = data[idx+1:]
		}
	}
	out := make([]byte, len(data))
	copy(out, data)
	return out
}

type rotatingWriter struct {
	mu       *sync.Mutex
	path     string
	file     **os.File
	size     int64
	maxBytes int64
	onRotate func(*os.File)
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if *w.file == nil {
		return 0, os.ErrClosed
	}

	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			fmt.Fprintf(os.Stderr, "Error rotating log file %s: %v\n", w.path, err)
		}
	}

	n, err := (*w.file).Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotateLocked() error {
	if *w.file != nil {
		_ = (*w.file).Close()
		*w.file = nil
	}
	backup := w.path + ".1"
	_ = os.Remove(backup)
	_ = os.Rename(w.path, backup)

	ff, err := os.OpenFile(w.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	*w.file = ff
	w.size = 0
	if w.onRotate != nil {
		w.onRotate(ff)
	}
	return nil
}

func Init(path, webpath string) {
	logMu.Lock()
	defer logMu.Unlock()

	logPath = path
	webLogPath = webpath
	maxBytes := maxLogBytes

	shared := path != "" && webpath != "" && filepath.Clean(path) == filepath.Clean(webpath)
	if path != "" {
		path = filepath.Clean(path)
		logPath = path
	}
	if webpath != "" {
		webpath = filepath.Clean(webpath)
		webLogPath = webpath
	}

	if shared {
		ff, size, err := openLogFile(path, maxBytes)
		if err != nil {
			fmt.Fprintf(origStderr, "Error create log file: %v\n", err)
			log.SetOutput(io.MultiWriter(ringBuf, origStderr))
			return
		}
		logFile = ff
		webLogFile = ff
		rw := &rotatingWriter{
			mu:       &logMu,
			path:     path,
			file:     &logFile,
			size:     size,
			maxBytes: maxBytes,
			onRotate: func(newF *os.File) {
				webLogFile = newF
				if err := redirectStdFDs(newF); err != nil {
					fmt.Fprintf(origStderr, "Error redirect stdout/stderr: %v\n", err)
				}
			},
		}
		webLog = log.New(io.MultiWriter(ringBuf, rw), " ", log.LstdFlags)
		applyServerLog(ff, rw)
		return
	}

	if webpath != "" {
		ff, size, err := openLogFile(webpath, maxBytes)
		if err != nil {
			fmt.Fprintf(origStderr, "Error create web log file: %v\n", err)
		} else {
			webLogFile = ff
			wrw := &rotatingWriter{
				mu:       &logMu,
				path:     webpath,
				file:     &webLogFile,
				size:     size,
				maxBytes: maxBytes,
			}
			webLog = log.New(wrw, " ", log.LstdFlags)
		}
	}

	if path != "" {
		ff, size, err := openLogFile(path, maxBytes)
		if err != nil {
			fmt.Fprintf(origStderr, "Error create log file: %v\n", err)
			log.SetOutput(io.MultiWriter(ringBuf, origStderr))
			return
		}
		logFile = ff
		rw := &rotatingWriter{
			mu:       &logMu,
			path:     path,
			file:     &logFile,
			size:     size,
			maxBytes: maxBytes,
			onRotate: func(newF *os.File) {
				if err := redirectStdFDs(newF); err != nil {
					fmt.Fprintf(origStderr, "Error redirect stdout/stderr: %v\n", err)
				}
			},
		}
		applyServerLog(ff, rw)
	} else {
		log.SetFlags(log.LstdFlags | log.LUTC | log.Lmsgprefix)
		log.SetPrefix("UTC0 ")
		log.SetOutput(io.MultiWriter(ringBuf, os.Stderr))
	}
}

func openLogFile(path string, maxBytes int64) (*os.File, int64, error) {
	var curSize int64
	if fi, err := os.Lstat(path); err == nil {
		if maxBytes > 0 && fi.Size() >= maxBytes {
			backup := path + ".1"
			_ = os.Remove(backup)
			_ = os.Rename(path, backup)
		} else {
			curSize = fi.Size()
		}
	}
	ff, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return nil, 0, err
	}
	return ff, curSize, nil
}

func applyServerLog(ff *os.File, rw io.Writer) {
	if err := redirectStdFDs(ff); err != nil {
		fmt.Fprintf(origStderr, "Error redirect stdout/stderr: %v\n", err)
	}
	log.SetFlags(log.LstdFlags | log.LUTC | log.Lmsgprefix)
	log.SetPrefix("UTC0 ")
	log.SetOutput(io.MultiWriter(ringBuf, rw))
}

func Close() {
	logMu.Lock()
	defer logMu.Unlock()

	os.Stdout = origStdout
	os.Stderr = origStderr
	log.SetOutput(origStderr)
	if logFile != nil {
		_ = logFile.Close()
		if webLogFile == logFile {
			webLogFile = nil
			webLog = nil
		}
		logFile = nil
	}
	if webLogFile != nil {
		_ = webLogFile.Close()
		webLogFile = nil
		webLog = nil
	}
	logPath = ""
	webLogPath = ""
}

// ReadLogs returns up to maxBytes of the most recent server logs.
// When a log file is configured, it reads from the rotated backup (.1) and current log file;
// otherwise (or if the file is empty), it returns from the in-memory ring buffer.
func ReadLogs(maxBytes int) []byte {
	if maxBytes <= 0 {
		maxBytes = DefaultRingBytes
	}

	logMu.Lock()
	p := logPath
	logMu.Unlock()

	if p != "" {
		if data := readFileTailPair(p+".1", p, maxBytes); len(data) > 0 {
			return data
		}
	}
	return ringBuf.Bytes(maxBytes)
}

func readFileTailPair(olderPath, newerPath string, maxBytes int) []byte {
	newer := readTailBytes(newerPath, maxBytes)
	if len(newer) >= maxBytes {
		return trimTailLines(newer, maxBytes)
	}
	rem := maxBytes - len(newer)
	older := readTailBytes(olderPath, rem)
	if len(older) == 0 {
		return newer
	}
	combined := make([]byte, 0, len(older)+len(newer))
	combined = append(combined, older...)
	combined = append(combined, newer...)
	return trimTailLines(combined, maxBytes)
}

func readTailBytes(path string, maxBytes int) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil || fi.Size() <= 0 {
		return nil
	}
	size := fi.Size()
	start := int64(0)
	if int64(maxBytes) < size {
		start = size - int64(maxBytes)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	if start > 0 {
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 && idx+1 < len(data) {
			data = data[idx+1:]
		}
	}
	return data
}

func TLogln(v ...interface{}) {
	log.Println(v...)
}

func WebLogln(v ...interface{}) {
	if webLog != nil {
		webLog.Println(v...)
	}
}

func WebLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		if webLog == nil {
			c.Next()
			return
		}
		body := ""
		// save body if not form or file
		if !strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
			bodyBytes, _ := io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
			body = string(bodyBytes)
		} else {
			body = "body hidden, too large"
		}
		c.Next()

		statusCode := c.Writer.Status()
		clientIP := c.ClientIP()
		method := c.Request.Method
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery
		if raw != "" {
			path = path + "?" + raw
		}

		logStr := fmt.Sprintf("%3d | %12s | %-7s %#v %v",
			statusCode,
			clientIP,
			method,
			path,
			body,
		)
		WebLogln(logStr)
	}
}
