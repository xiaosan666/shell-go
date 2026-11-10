package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"shellgo/internal/audit"
	"shellgo/internal/files"
)

type fileReq struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Offset    int64  `json:"offset"`
	Size      int    `json:"size"`
	Total     int64  `json:"total"`
	Version   string `json:"version"` // 纳秒 mtime 的字符串形式：JS 的 Number 存不下 19 位整数
	Data      string `json:"data"`
	Recursive bool   `json:"recursive"`
}

func (s *Server) fileManager(c *call) (*files.Manager, bool) {
	if !s.files.Enabled() {
		c.fail(codeBusiness, "文件管理未启用")
		return nil, false
	}
	return s.files, true
}

func (s *Server) handleFileList(c *call) {
	if !c.need(c.caps.FilesRead, "当前账号没有文件浏览权限") {
		return
	}
	var req fileReq
	if !c.bind(&req) {
		return
	}
	m, ok := s.fileManager(c)
	if !ok {
		return
	}
	clean, entries, err := m.List(req.Path)
	if err != nil {
		s.fileError(c, err)
		return
	}
	parent := ""
	if clean != "" {
		parent = path.Dir(clean)
		if parent == "." {
			parent = ""
		}
	}
	c.ok(map[string]any{
		"path":    clean,
		"parent":  parent,
		"root":    m.Root(),
		"entries": entries,
		// 全局开关与账号权限取交集：配置允许写、账号也有写权限才算能写
		"writable":  m.Writable() && c.caps.FilesWrite,
		"deletable": m.Deletable() && c.caps.FilesDelete,
		"maxUpload": m.MaxUpload(),
	})
}

func (s *Server) handleFileRead(c *call) {
	if !c.need(c.caps.FilesRead, "当前账号没有文件下载权限") {
		return
	}
	var req fileReq
	if !c.bind(&req) {
		return
	}
	m, ok := s.fileManager(c)
	if !ok {
		return
	}
	f, fi, err := m.OpenRead(req.Path)
	if err != nil {
		s.fileError(c, err)
		return
	}
	defer f.Close()

	data, eof, err := files.ReadChunk(f, req.Offset, req.Size)
	if err != nil {
		s.fileError(c, err)
		return
	}
	if req.Offset == 0 {
		s.audit.Log(audit.Event{Event: "file_download", User: c.user, IP: c.ip,
			Detail: req.Path + " (" + humanSize(fi.Size()) + ")"})
	}
	c.ok(map[string]any{
		"data":   base64.StdEncoding.EncodeToString(data),
		"offset": req.Offset + int64(len(data)),
		"eof":    eof,
		"size":   fi.Size(),
	})
}

func (s *Server) handleFileWrite(c *call) {
	if !c.need(c.caps.FilesWrite, "当前账号没有文件写入权限") {
		return
	}
	var req fileReq
	if !c.bind(&req) {
		return
	}
	m, ok := s.fileManager(c)
	if !ok {
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		c.fail(codeBadReq, "数据不是合法 base64")
		return
	}
	next, err := m.WriteChunk(req.Path, req.Offset, req.Total, data)
	if err != nil {
		s.fileError(c, err)
		return
	}
	if req.Offset == 0 {
		s.audit.Log(audit.Event{Event: "file_upload", User: c.user, IP: c.ip,
			Detail: req.Path + " (" + humanSize(req.Total) + ")"})
	}
	c.ok(map[string]any{"offset": next, "done": next >= req.Total})
}

// handleFileStat 返回单个文件的元信息。
// 前端用它做两件事：上传前判断能否续传（已有多少字节）、编辑保存前记录 mtime 做冲突检测。
func (s *Server) handleFileStat(c *call) {
	if !c.need(c.caps.FilesRead, "当前账号没有文件浏览权限") {
		return
	}
	var req fileReq
	if !c.bind(&req) {
		return
	}
	m, ok := s.fileManager(c)
	if !ok {
		return
	}
	e, mtimeNS, err := m.StatDetail(req.Path)
	if err != nil {
		s.fileError(c, err)
		return
	}
	c.ok(map[string]any{
		"entry":   e,
		"version": strconv.FormatInt(mtimeNS, 10),
		"maxEdit": m.MaxEdit(),
	})
}

// handleFileSave 在线编辑保存：整文件覆盖 + 乐观并发控制。
func (s *Server) handleFileSave(c *call) {
	if !c.need(c.caps.FilesWrite, "当前账号没有文件写入权限") {
		return
	}
	var req fileReq
	if !c.bind(&req) {
		return
	}
	m, ok := s.fileManager(c)
	if !ok {
		return
	}
	data, err := base64.StdEncoding.DecodeString(req.Data)
	if err != nil {
		c.fail(codeBadReq, "数据不是合法 base64")
		return
	}
	size, mtimeNS, err := m.SaveText(req.Path, data, parseVersion(req.Version))
	if err != nil {
		s.fileError(c, err)
		return
	}
	s.audit.Log(audit.Event{Event: "file_edit", User: c.user, IP: c.ip,
		Detail: req.Path + " (" + humanSize(size) + ")"})
	c.ok(map[string]any{"size": size, "version": strconv.FormatInt(mtimeNS, 10)})
}

// parseVersion 解析客户端带回的版本号；空串或非法值一律视作 0（即「新建」）。
func parseVersion(v string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (s *Server) handleFileMkdir(c *call) {
	if !c.need(c.caps.FilesWrite, "当前账号没有文件写入权限") {
		return
	}
	var req fileReq
	if !c.bind(&req) {
		return
	}
	m, ok := s.fileManager(c)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		c.fail(codeBadReq, "目录名不能为空")
		return
	}
	if err := m.Mkdir(req.Path); err != nil {
		s.fileError(c, err)
		return
	}
	s.audit.Log(audit.Event{Event: "file_mkdir", User: c.user, IP: c.ip, Detail: req.Path})
	c.ok(nil)
}

func (s *Server) handleFileRename(c *call) {
	if !c.need(c.caps.FilesWrite, "当前账号没有文件写入权限") {
		return
	}
	var req fileReq
	if !c.bind(&req) {
		return
	}
	m, ok := s.fileManager(c)
	if !ok {
		return
	}
	next, err := m.Rename(req.Path, req.Name)
	if err != nil {
		s.fileError(c, err)
		return
	}
	s.audit.Log(audit.Event{Event: "file_rename", User: c.user, IP: c.ip,
		Detail: req.Path + " -> " + next})
	c.ok(map[string]any{"path": next})
}

func (s *Server) handleFileRemove(c *call) {
	if !c.need(c.caps.FilesDelete, "当前账号没有文件删除权限") {
		return
	}
	var req fileReq
	if !c.bind(&req) {
		return
	}
	m, ok := s.fileManager(c)
	if !ok {
		return
	}
	if err := m.Remove(req.Path, req.Recursive); err != nil {
		s.fileError(c, err)
		return
	}
	s.audit.Log(audit.Event{Event: "file_delete", User: c.user, IP: c.ip, Detail: req.Path})
	c.ok(nil)
}

func (s *Server) fileError(c *call, err error) {
	switch {
	case errors.Is(err, files.ErrForbidden):
		c.fail(codeBusiness, "路径超出允许的根目录")
	case errors.Is(err, files.ErrReadOnly):
		c.fail(codeBusiness, "服务端配置为只读，不允许写入")
	case errors.Is(err, files.ErrNoDelete):
		c.fail(codeBusiness, "服务端配置禁止删除")
	case errors.Is(err, files.ErrTooLarge):
		c.fail(codeBusiness, "文件超过服务端允许的大小上限")
	case errors.Is(err, files.ErrIsDir):
		c.fail(codeBusiness, "目标是目录，无法下载")
	case errors.Is(err, files.ErrConflict):
		c.fail(codeBusiness, "文件在你编辑期间已被修改，请重新打开后再保存")
	case errors.Is(err, files.ErrBinary):
		c.fail(codeBusiness, "疑似二进制文件，不能用文本编辑器打开")
	case errors.Is(err, files.ErrNotDir):
		c.fail(codeBusiness, "目标不是目录")
	case errors.Is(err, fs.ErrNotExist):
		c.fail(codeBusiness, "文件或目录不存在")
	case errors.Is(err, fs.ErrPermission):
		c.fail(codeBusiness, "权限不足，服务运行账号无法访问该路径")
	default:
		c.fail(codeBusiness, "操作失败："+err.Error())
	}
}

func humanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	v := float64(n) / 1024
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
