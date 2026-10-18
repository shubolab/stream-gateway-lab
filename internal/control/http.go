package control

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"stream-gateway-lab/internal/gateway"
	"stream-gateway-lab/internal/transport"
)

//go:embed web/* install.sh
var assets embed.FS

const (
	LinuxBinary  = "stream-gateway-executor-linux-amd64"
	DarwinBinary = "stream-gateway-executor-darwin-arm64"
)

var executorBuilds = map[string]string{"linux/amd64": LinuxBinary, "darwin/arm64": DarwinBinary}

func (s *Server) newRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Cache-Control", "no-store")
		c.Next()
	})
	r.SetTrustedProxies(nil)
	web, _ := fs.Sub(assets, "web")
	page := func(c *gin.Context) {
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		http.FileServer(http.FS(web)).ServeHTTP(c.Writer, c.Request)
	}
	r.GET("/", page)
	r.GET("/app.js", page)
	r.GET("/style.css", page)
	r.GET("/vendor/*filepath", page)
	r.GET("/ws", s.socket)
	r.GET("/install.sh", func(c *gin.Context) {
		data, _ := assets.ReadFile("install.sh")
		c.Data(200, "text/x-shellscript; charset=utf-8", data)
	})
	for _, binary := range executorBuilds {
		r.GET("/downloads/"+binary, s.download)
		r.GET("/downloads/"+binary+".gz", s.download)
		r.GET("/downloads/"+binary+".sha256", s.download)
	}
	r.POST("/api/register", s.register)
	api := r.Group("/api", s.authenticate)
	api.GET("/state", s.state)
	api.GET("/model-config", s.getModelConfig)
	api.PUT("/model-config", s.setModelConfig)
	api.POST("/enrollments", s.enroll)
	api.POST("/tasks", s.dispatch)
	api.POST("/tasks/:id/cancel", s.cancelTask)
	api.POST("/sessions", s.createSession)
	api.GET("/sessions/:id", s.sessionDetail)
	api.POST("/sessions/:id/messages", s.sendMessage)
	r.NoRoute(func(c *gin.Context) { fail(c, 404, "not found") })
	return r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.router.ServeHTTP(w, r) }

func (s *Server) authenticate(c *gin.Context) {
	if subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), []byte("Bearer "+s.config.AdminToken)) != 1 {
		fail(c, 401, "请输入有效的管理密钥")
		return
	}
	c.Next()
}

func (s *Server) socket(c *gin.Context) {
	s.mu.Lock()
	m := s.members[c.Query("pair")]
	var route *gateway.PairRoute
	if m != nil && !s.closed {
		route = m.route
	}
	s.mu.Unlock()
	if route == nil {
		fail(c, 401, "invalid endpoint credentials")
		return
	}
	route.ServeHTTP(websocketWriter{c.Writer}, c.Request)
}

// coder/websocket 在 Hijack 前写出 101；Gin 1.11 的 Hijack 会拒绝已写响应。
// 保留 Gin 的状态记录，只让连接接管直接作用于底层 ResponseWriter。
type websocketWriter struct{ gin.ResponseWriter }

func (w websocketWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	underlying := w.ResponseWriter.(interface{ Unwrap() http.ResponseWriter }).Unwrap()
	return http.NewResponseController(underlying).Hijack()
}

func (s *Server) state(c *gin.Context) {
	executors, tasks := s.snapshot()
	builds := make(map[string]bool, len(executorBuilds))
	for platform, binary := range executorBuilds {
		info, err := os.Stat(filepath.Join(s.config.DownloadDir, binary))
		compressed, compressedErr := os.Stat(filepath.Join(s.config.DownloadDir, binary+".gz"))
		builds[platform] = s.config.DownloadDir != "" && err == nil && info.Mode().IsRegular() && compressedErr == nil && compressed.Mode().IsRegular()
	}
	c.JSON(200, gin.H{"executors": executors, "tasks": tasks, "sessions": s.sessionSnapshots(), "model_ready": s.modelReady(), "downloads": builds, "download_ready": builds["linux/amd64"]})
}

func (s *Server) enroll(c *gin.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, expiry := range s.joins {
		if time.Now().After(expiry) {
			delete(s.joins, token)
		}
	}
	if s.closed || len(s.joins) >= 100 {
		fail(c, 503, "registration unavailable")
		return
	}
	token, expiry := transport.NewID(), time.Now().Add(15*time.Minute)
	s.joins[token] = expiry
	c.JSON(201, gin.H{"token": token, "expires_at": expiry})
}

func (s *Server) dispatch(c *gin.Context) {
	var input TaskInput
	if !readJSON(c, &input) {
		return
	}
	task, status, err := s.createTask(input)
	if err != nil {
		fail(c, status, err.Error())
		return
	}
	c.JSON(status, task)
}

func (s *Server) cancelTask(c *gin.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.tasks[c.Param("id")]
	if run == nil {
		fail(c, 404, "task not found")
		return
	}
	if run.FinishedAt != nil {
		fail(c, 409, "task already finished")
		return
	}
	run.cancel()
	c.JSON(202, gin.H{"status": "cancel_requested"})
}

func (s *Server) register(c *gin.Context) {
	var input struct {
		Name     string `json:"name"`
		Hostname string `json:"hostname"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
	}
	if !readJSON(c, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 80 || len(input.Hostname) > 255 || input.OS == "" || len(input.OS) > 32 || input.Arch == "" || len(input.Arch) > 32 {
		fail(c, 400, "invalid executor name or machine information")
		return
	}
	join := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.joins[join]
	if !ok || time.Now().After(expiry) || s.closed {
		fail(c, 401, "registration code expired or already used")
		return
	}
	id, token := transport.NewID(), transport.NewID()
	e := Executor{ID: id, Name: input.Name, Hostname: input.Hostname, OS: input.OS, Arch: input.Arch, RegisteredAt: time.Now().UTC()}
	s.members[id] = &member{identity: identity{Executor: e, Token: token}, route: gateway.NewPoolRoute(id, token)}
	if err := s.saveMembersLocked(); err != nil {
		delete(s.members, id)
		fail(c, 500, "could not save executor registration")
		return
	}
	delete(s.joins, join)
	c.JSON(201, gin.H{"id": id, "token": token})
}

func (s *Server) download(c *gin.Context) {
	binary := strings.TrimSuffix(filepath.Base(c.Request.URL.Path), ".sha256")
	if s.config.DownloadDir == "" {
		fail(c, 503, "Executor build is not available; run scripts/build-distribution.sh")
		return
	}
	path := filepath.Join(s.config.DownloadDir, binary)
	f, err := os.Open(path)
	if err != nil {
		fail(c, 503, "Executor build is not available; run scripts/build-distribution.sh")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		fail(c, 503, "Executor build unavailable")
		return
	}
	if strings.HasSuffix(c.Request.URL.Path, ".sha256") {
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			fail(c, 500, "could not checksum build")
			return
		}
		c.String(200, hex.EncodeToString(h.Sum(nil))+"  "+binary+"\n")
		return
	}
	c.Header("Content-Type", "application/octet-stream")
	http.ServeContent(c.Writer, c.Request, binary, info.ModTime(), f)
}

func readJSON(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		fail(c, 400, "invalid JSON: "+err.Error())
		return false
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		fail(c, 400, "expected one JSON object")
		return false
	}
	return true
}

func fail(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": message})
}
