// Package captcha 生成滑动拼图验证码。
//
// 服务端随即绘制一张抽象背景图，在上面挖出一个拼图形状的缺口，
// 并把缺口处的像素裁出来作为可拖动的拼图块。客户端只拿到两张图，
// 缺口的真实横坐标（答案）留在服务端；用户拖动结束后只上报水平位移，
// 由服务端比对位移与答案的偏差是否在容差内。
//
// 一次「下发 + 校验」只能消耗一次：校验不通过或超时都需要重新取图。
package captcha

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/png"
	"math"
	"math/rand"
	"sync"
	"time"

	"shellgo/internal/crypto"
)

// 图片尺寸即前端展示尺寸（CSS 像素），1:1 对应，方便位移比对。
const (
	ImageW = 304
	ImageH = 136

	// PieceSize 是拼图主方块的边长，PieceBump 是顶部凸起的半径。
	PieceSize = 40.0
	PieceBump = 6.5
	piecePad  = 1.0

	// Tolerance 是允许的像素偏差，太严会让人反复失败，太松可以穷举。
	Tolerance = 6

	challengeTTL = 2 * time.Minute
	ticketTTL    = 2 * time.Minute

	// maxPending 限制内存里同时存在的挑战数，防止被刷爆。
	maxPending = 4096
)

var (
	// ErrBusy 表示待校验的验证码太多，稍后再试。
	ErrBusy = errors.New("验证码服务繁忙")
	// ErrExpired 表示验证码不存在或已过期。
	ErrExpired = errors.New("验证码已过期，请刷新")
	// ErrMismatch 表示拖动的落点与缺口位置不符。
	ErrMismatch = errors.New("拼图未对齐")
	// ErrTicket 表示验证票据无效或已被使用。
	ErrTicket = errors.New("验证结果无效或已过期")
)

// Challenge 是下发给前端的一次验证码素材。
type Challenge struct {
	ID    string `json:"cid"`
	Bg    string `json:"bg"`    // data URI
	Piece string `json:"piece"` // data URI
	W     int    `json:"w"`     // 背景宽（图片像素）
	H     int    `json:"h"`     // 背景高
	PW    int    `json:"pw"`    // 拼图块宽
	PH    int    `json:"ph"`    // 拼图块高
	PY    int    `json:"py"`    // 拼图块在背景坐标系里的 top 偏移
	Tol   int    `json:"tol"`   // 容差（像素）
}

type pending struct {
	x   int // 缺口左侧横坐标，即拖动到位的位移
	exp time.Time
}

type Manager struct {
	mu      sync.Mutex
	pending map[string]*pending
	tickets map[string]time.Time
}

func New() *Manager {
	return &Manager{
		pending: map[string]*pending{},
		tickets: map[string]time.Time{},
	}
}

// Issue 生成一张新的验证码。
func (m *Manager) Issue() (*Challenge, error) {
	raw, err := render()
	if err != nil {
		return nil, err
	}
	id := crypto.RandomHex(16)

	m.mu.Lock()
	m.expireLocked()
	if len(m.pending) >= maxPending {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	m.pending[id] = &pending{x: raw.x, exp: time.Now().Add(challengeTTL)}
	m.mu.Unlock()

	return &Challenge{
		ID:    id,
		Bg:    dataURI(raw.bg),
		Piece: dataURI(raw.piece),
		W:     ImageW,
		H:     ImageH,
		PW:    raw.pw,
		PH:    raw.ph,
		PY:    raw.py,
		Tol:   Tolerance,
	}, nil
}

// Verify 校验一次拖动结果。无论成功与否，该验证码都会被消耗掉。
// 成功时返回一张一次性票据，登录时需要带上。
func (m *Manager) Verify(id string, x int) (string, error) {
	if id == "" {
		return "", ErrExpired
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.pending[id]
	if !ok {
		return "", ErrExpired
	}
	delete(m.pending, id) // 一次性：猜错就要重新取图，避免反复试探
	if time.Now().After(it.exp) {
		return "", ErrExpired
	}
	diff := x - it.x
	if diff < 0 {
		diff = -diff
	}
	if diff > Tolerance {
		return "", ErrMismatch
	}
	tk := crypto.RandomHex(24)
	m.tickets[tk] = time.Now().Add(ticketTTL)
	return tk, nil
}

// Consume 在登录时核销票据。
func (m *Manager) Consume(ticket string) error {
	if ticket == "" {
		return ErrTicket
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.tickets[ticket]
	if !ok {
		return ErrTicket
	}
	delete(m.tickets, ticket)
	if time.Now().After(exp) {
		return ErrTicket
	}
	return nil
}

// Sweep 清理过期数据，由外部定时调用。
func (m *Manager) Sweep() {
	m.mu.Lock()
	m.expireLocked()
	m.mu.Unlock()
}

func (m *Manager) expireLocked() {
	now := time.Now()
	for k, v := range m.pending {
		if now.After(v.exp) {
			delete(m.pending, k)
		}
	}
	for k, t := range m.tickets {
		if now.After(t) {
			delete(m.tickets, k)
		}
	}
}

// ---------- 绘图 ----------

type rendered struct {
	bg, piece []byte
	pw, ph    int
	py        int // 拼图块 top 偏移（背景坐标系）
	x         int // 缺口左侧横坐标
}

func render() (*rendered, error) {
	bg := drawScene(ImageW, ImageH)

	pw := int(math.Ceil(PieceSize + 2*piecePad))
	ph := int(math.Ceil(PieceSize + PieceBump + 2*piecePad))
	bumpOff := int(math.Round(PieceBump))

	// 缺口要留边，既好看也保证拼图块拖得到位
	minX, maxX := 12, ImageW-pw-12
	hx := minX + rand.Intn(maxX-minX+1)
	minY, maxY := bumpOff+6, ImageH-ph-6
	hy := minY + rand.Intn(maxY-minY+1) // 主方块顶边

	py := hy - bumpOff
	piece := cutPiece(bg, hx, py, pw, ph)
	drawHole(bg, hx, py, pw, ph)

	bgPNG, err := encodePNG(bg)
	if err != nil {
		return nil, err
	}
	piecePNG, err := encodePNG(piece)
	if err != nil {
		return nil, err
	}
	return &rendered{bg: bgPNG, piece: piecePNG, pw: pw, ph: ph, py: py, x: hx}, nil
}

func encodePNG(img *image.RGBA) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func dataURI(pngBytes []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes)
}

// palettes 是「起点色 → 终点色 → 高光色」的配色组，都是低饱和的暗色调，
// 与登录页整体的深色玻璃质感一致。
var palettes = [][3][3]float64{
	{{8, 18, 34}, {18, 74, 92}, {150, 235, 220}},
	{{16, 10, 38}, {86, 28, 132}, {205, 175, 255}},
	{{6, 20, 28}, {26, 82, 108}, {165, 220, 255}},
	{{28, 8, 24}, {120, 42, 80}, {255, 185, 205}},
	{{6, 24, 22}, {18, 82, 58}, {170, 240, 200}},
	{{10, 14, 32}, {48, 62, 138}, {190, 205, 255}},
	{{22, 12, 10}, {108, 46, 28}, {250, 190, 140}},
	{{10, 16, 20}, {40, 70, 74}, {210, 240, 235}},
}

func drawScene(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	p := palettes[rand.Intn(len(palettes))]
	c1, c2, hi := p[0], p[1], p[2]

	// 对角渐变打底
	den := float64(h)*0.55 + float64(w)*0.9
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := (float64(y)*0.55 + float64(maxInt(w-x, 0))*0.9) / den
			setPix(img, x, y, c1[0]+(c2[0]-c1[0])*t, c1[1]+(c2[1]-c1[1])*t, c1[2]+(c2[2]-c1[2])*t)
		}
	}

	// 散景光斑：制造纹理，防止靠纯色块猜位置
	n := 20 + rand.Intn(12)
	for i := 0; i < n; i++ {
		cx := rand.Float64() * float64(w)
		cy := rand.Float64() * float64(h)
		r := 10 + rand.Float64()*40
		a := 0.06 + rand.Float64()*0.16
		for y := int(cy - r); y <= int(cy+r); y++ {
			for x := int(cx - r); x <= int(cx+r); x++ {
				if x < 0 || y < 0 || x >= w || y >= h {
					continue
				}
				d := math.Hypot(float64(x)-cx, float64(y)-cy)
				if d > r {
					continue
				}
				k := 1 - d/r
				// 中心亮、边缘留一圈光环，比纯衰减更有层次
				v := a * k * k
				if k > 0.55 && k < 0.85 {
					v *= 1.6
				}
				blend(img, x, y, hi[0], hi[1], hi[2], v)
			}
		}
	}

	// 斜向光带
	for i := 0; i < 3; i++ {
		ang := -0.85 + rand.Float64()*0.7
		nx, ny := math.Cos(ang), math.Sin(ang)
		off := rand.Float64() * float64(h)
		width := 10 + rand.Float64()*26
		a := 0.03 + rand.Float64()*0.06
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				d := math.Abs(nx*(float64(x)-float64(w)/2) + ny*(float64(y)-off))
				if d > width {
					continue
				}
				k := 1 - d/width
				blend(img, x, y, 255, 255, 255, a*k*k)
			}
		}
	}

	// 细斜线：给背景一点结构感，光斑之外还有可辨识的纹理
	for i := 0; i < 2; i++ {
		ang := -0.5 + rand.Float64()*1.0
		dx, dy := math.Cos(ang), math.Sin(ang)
		off := rand.Float64() * float64(h)
		a := 0.06 + rand.Float64()*0.08
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				d := math.Abs(-dy*(float64(x)-float64(w)/2) + dx*(float64(y)-off))
				if d > 1.2 {
					continue
				}
				blend(img, x, y, 255, 255, 255, a*(1-d/1.2))
			}
		}
	}

	// 暗角 + 细颗粒
	cxm, cym := float64(w)/2, float64(h)/2
	maxD := math.Hypot(cxm, cym)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			d := math.Hypot(float64(x)-cxm, float64(y)-cym) / maxD
			if d > 0.5 {
				k := (d - 0.5) / 0.5
				blend(img, x, y, 0, 0, 0, 0.6*k*k)
			}
			if rand.Intn(4) == 0 {
				g := float64(rand.Intn(256))
				blend(img, x, y, g, g, g, 0.045)
			}
		}
	}
	return img
}

// cutPiece 把背景上缺口处的像素裁成一块带透明通道的拼图，并在边缘描亮边。
func cutPiece(src *image.RGBA, hx, py, pw, ph int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, pw, ph))
	for j := 0; j < ph; j++ {
		for i := 0; i < pw; i++ {
			lx, ly := localOf(i, j)
			cov := coverage(lx, ly)
			if cov <= 0 {
				continue
			}
			r, g, b := at(src, hx-int(piecePad)+i, py+j)
			if cov < 0.92 {
				// 边缘混一点白，拼图块轮廓才看得清
				k := 1 - cov/0.92
				r = r + (250-r)*0.7*k
				g = g + (250-g)*0.7*k
				b = b + (250-b)*0.7*k
			}
			o := out.PixOffset(i, j)
			out.Pix[o] = clamp8(r)
			out.Pix[o+1] = clamp8(g)
			out.Pix[o+2] = clamp8(b)
			out.Pix[o+3] = clamp8(cov * 255)
		}
	}
	return out
}

// drawHole 在背景上把缺口压暗并描边，做出「被挖掉一块」的观感。
func drawHole(bg *image.RGBA, hx, py, pw, ph int) {
	for j := 0; j < ph; j++ {
		for i := 0; i < pw; i++ {
			lx, ly := localOf(i, j)
			cov := coverage(lx, ly)
			if cov <= 0 {
				continue
			}
			bx, by := hx-int(piecePad)+i, py+j
			r, g, b := at(bg, bx, by)
			// 压暗但保留一点原本的纹理，免得变成一块死黑
			nr := r*0.30 + 10
			ng := g*0.30 + 12
			nb := b*0.32 + 16
			if cov < 0.85 {
				// 缺口边缘描白，界线清楚才拖得准
				k := 1 - cov/0.85
				nr = nr + (250-nr)*0.85*k
				ng = ng + (252-ng)*0.85*k
				nb = nb + (252-nb)*0.85*k
			}
			setPix(bg, bx, by, r+(nr-r)*cov, g+(ng-g)*cov, b+(nb-b)*cov)
		}
	}
}

// localOf 把拼图块图片的像素坐标换算成形状的局部坐标：
// 主方块左上角是 (0,0)，顶部凸起落在负 y 区间。
func localOf(i, j int) (float64, float64) {
	return float64(i) + 0.5 - piecePad, float64(j) + 0.5 - (PieceBump + piecePad)
}

// inShape 判断局部坐标是否落在拼图形状内：
// 圆角方块 ∪ 顶部圆形凸起 − 右侧圆形凹口。
func inShape(lx, ly float64) bool {
	const s = PieceSize
	const dentR = PieceBump * 0.95
	const corner = PieceSize * 0.14

	if !inRoundRect(lx, ly, 0, 0, s, s, corner) && !inCircle(lx, ly, s/2, 0, PieceBump) {
		return false
	}
	return !inCircle(lx, ly, s, s/2, dentR)
}

func inRoundRect(x, y, x0, y0, w, h, r float64) bool {
	if x < x0 || y < y0 || x > x0+w || y > y0+h {
		return false
	}
	cx := math.Min(math.Max(x, x0+r), x0+w-r)
	cy := math.Min(math.Max(y, y0+r), y0+h-r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

func inCircle(x, y, cx, cy, r float64) bool {
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

// coverage 用 3×3 超采样估算像素覆盖率，得到平滑的抗锯齿边缘。
func coverage(lx, ly float64) float64 {
	const n = 3
	hit := 0
	for a := 0; a < n; a++ {
		for b := 0; b < n; b++ {
			x := lx + (float64(a)+0.5)/n - 0.5
			y := ly + (float64(b)+0.5)/n - 0.5
			if inShape(x, y) {
				hit++
			}
		}
	}
	return float64(hit) / (n * n)
}

// ---------- 像素小工具 ----------

func at(img *image.RGBA, x, y int) (float64, float64, float64) {
	if x < 0 || y < 0 || x >= img.Rect.Dx() || y >= img.Rect.Dy() {
		return 0, 0, 0
	}
	o := img.PixOffset(x, y)
	return float64(img.Pix[o]), float64(img.Pix[o+1]), float64(img.Pix[o+2])
}

func setPix(img *image.RGBA, x, y int, r, g, b float64) {
	if x < 0 || y < 0 || x >= img.Rect.Dx() || y >= img.Rect.Dy() {
		return
	}
	o := img.PixOffset(x, y)
	img.Pix[o] = clamp8(r)
	img.Pix[o+1] = clamp8(g)
	img.Pix[o+2] = clamp8(b)
	img.Pix[o+3] = 255
}

// blend 按 alpha 把颜色叠到已有像素上（0..255 空间，非预乘）。
func blend(img *image.RGBA, x, y int, r, g, b, a float64) {
	if x < 0 || y < 0 || x >= img.Rect.Dx() || y >= img.Rect.Dy() || a <= 0 {
		return
	}
	if a > 1 {
		a = 1
	}
	o := img.PixOffset(x, y)
	img.Pix[o] = clamp8(float64(img.Pix[o])*(1-a) + r*a)
	img.Pix[o+1] = clamp8(float64(img.Pix[o+1])*(1-a) + g*a)
	img.Pix[o+2] = clamp8(float64(img.Pix[o+2])*(1-a) + b*a)
	img.Pix[o+3] = 255
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
