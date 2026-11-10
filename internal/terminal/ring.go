package terminal

import "sync"

// Ring 是带绝对偏移的滚动缓冲区，供断线重连与轮询模式回放使用。
type Ring struct {
	mu    sync.Mutex
	data  []byte
	base  uint64 // data[0] 对应的绝对偏移
	limit int
}

func NewRing(limit int) *Ring {
	if limit <= 0 {
		limit = 512 * 1024
	}
	return &Ring{limit: limit}
}

func (r *Ring) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.data = append(r.data, p...)
	if len(r.data) > r.limit {
		drop := len(r.data) - r.limit
		if cap(r.data) > 2*r.limit {
			nd := make([]byte, r.limit)
			copy(nd, r.data[drop:])
			r.data = nd
		} else {
			r.data = r.data[drop:]
		}
		r.base += uint64(drop)
	}
}

// Size 返回累计写入的字节数，也就是当前的绝对末尾偏移。
func (r *Ring) Size() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.base + uint64(len(r.data))
}

// ReadFrom 读取 cursor 之后的全部数据。reset 为 true 表示 cursor 已滚出缓冲，
// 调用方应清屏后以返回的数据作为全量快照。
func (r *Ring) ReadFrom(cursor uint64, max int) (data []byte, next uint64, reset bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end := r.base + uint64(len(r.data))
	if cursor < r.base {
		cursor = r.base
		reset = true
	}
	if cursor > end {
		return nil, end, true
	}
	off := cursor - r.base
	data = r.data[off:]
	if max > 0 && len(data) > max {
		// 只返回最新的 max 字节，同时推进 reset 语义由调用方处理
		trim := len(data) - max
		return append([]byte{}, data[trim:]...), end, true
	}
	return append([]byte{}, data...), end, false
}

// Tail 返回最近 max 字节（用于重连快照）。
func (r *Ring) Tail(max int) ([]byte, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := r.data
	if max > 0 && len(d) > max {
		d = d[len(d)-max:]
	}
	return append([]byte{}, d...), r.base + uint64(len(r.data))
}
