package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"shellgo/internal/audit"
	"shellgo/internal/terminal"
)

type recordReq struct {
	ID    string `json:"id"`
	From  int    `json:"from"`
	Count int    `json:"count"`
	Limit int    `json:"limit"`
}

func (s *Server) handleRecordList(c *call) {
	if !s.cfg.Record.Enabled {
		c.ok(map[string]any{"enabled": false, "records": []any{}})
		return
	}
	var req recordReq
	if !c.bind(&req) {
		return
	}
	list, err := terminal.ListRecords(s.cfg.Record.Dir)
	if err != nil {
		c.fail(codeInternal, "读取录制目录失败："+err.Error())
		return
	}
	// 没有 records_all 权限的账号只看自己的会话，不再把别人的录制列出来。
	if !c.caps.RecordsAll {
		own := make([]terminal.RecordMeta, 0, len(list))
		for _, m := range list {
			if m.User == c.user {
				own = append(own, m)
			}
		}
		list = own
	}
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	if len(list) > limit {
		list = list[:limit]
	}
	c.ok(map[string]any{
		"enabled": true,
		"records": list,
		"count":   len(list),
		"scope":   map[bool]string{true: "all", false: "own"}[c.caps.RecordsAll],
	})
}

func (s *Server) handleRecordGet(c *call) {
	if !s.cfg.Record.Enabled {
		c.fail(codeBusiness, "会话录制未启用")
		return
	}
	var req recordReq
	if !c.bind(&req) {
		return
	}
	// 先只读元信息做归属判断，避免为了验权把整个录制读一遍。
	meta, err := terminal.GetRecordMeta(s.cfg.Record.Dir, req.ID)
	if err != nil {
		c.fail(codeBusiness, "读取录制失败："+err.Error())
		return
	}
	if !c.need(c.caps.RecordsAll || meta.User == c.user, "只能查看自己创建的会话录制") {
		return
	}
	meta, events, next, eof, err := terminal.LoadRecord(s.cfg.Record.Dir, req.ID, req.From, req.Count)
	if err != nil {
		c.fail(codeBusiness, "读取录制失败："+err.Error())
		return
	}
	c.ok(map[string]any{
		"meta":   meta,
		"events": events,
		"next":   next,
		"eof":    eof,
	})
}

func (s *Server) handleRecordDelete(c *call) {
	if !s.cfg.Record.Enabled {
		c.fail(codeBusiness, "会话录制未启用")
		return
	}
	if !c.need(c.caps.RecordsDelete, "当前账号没有删除录制的权限") {
		return
	}
	var req recordReq
	if !c.bind(&req) {
		return
	}
	id := strings.TrimSuffix(strings.TrimSpace(req.ID), ".jsonl")
	if id == "" || strings.ContainsAny(id, `/\`) || strings.Contains(id, "..") {
		c.fail(codeBadReq, "录制 ID 非法")
		return
	}
	// 复用读取侧校验，同时拿到归属人，避免路径穿越与越权删除。
	meta, err := terminal.GetRecordMeta(s.cfg.Record.Dir, id)
	if err != nil {
		c.fail(codeBusiness, "录制不存在")
		return
	}
	if !c.need(c.caps.RecordsAll || meta.User == c.user, "只能删除自己创建的会话录制") {
		return
	}
	path := filepath.Join(s.cfg.Record.Dir, id+".jsonl")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.fail(codeBusiness, "删除失败："+err.Error())
		return
	}
	s.audit.Log(audit.Event{Event: "record_delete", User: c.user, IP: c.ip, Detail: id})
	c.ok(nil)
}
