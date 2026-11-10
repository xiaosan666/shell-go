// Package crypto 实现 web shell 的应用层加密信封。
//
// 设计目标：
//   - 密钥交换：服务端每次握手生成临时 SM2 密钥对，客户端用公钥加密 16 字节会话密钥
//   - 报文加密：SM4-CBC + PKCS#7，每条报文随机 16 字节 IV
//   - 报文认证：HMAC-SM3（encrypt-then-MAC），32 字节
//   - 传输格式：base64( IV | 密文 | MAC )
//
// 注意：gmsm 的 SM2 Decrypt 要求密文以 0x04 开头（未压缩点前缀），
// 而 JS 侧 sm-crypto 输出的 C1 不含该前缀，因此协议规定客户端必须补齐 0x04。
package crypto

import (
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"

	"github.com/tjfoc/gmsm/sm2"
	"github.com/tjfoc/gmsm/sm3"
	"github.com/tjfoc/gmsm/sm4"
)

const (
	KeyLen = 16
	IVLen  = 16
	MACLen = 32
)

var (
	ErrBadEnvelope = errors.New("报文格式错误")
	ErrBadMAC      = errors.New("报文完整性校验失败")
	ErrBadPadding  = errors.New("解密失败（填充非法）")
)

// DeriveKeys 由会话密钥派生出加密密钥与 MAC 密钥。
// encKey = SM3(K || 0x01) 前 16 字节；macKey = SM3(K || 0x02) 全部 32 字节。
func DeriveKeys(k []byte) (encKey, macKey []byte) {
	encKey = sum(append(append([]byte{}, k...), 0x01))[:KeyLen]
	macKey = sum(append(append([]byte{}, k...), 0x02))
	return
}

func sum(b []byte) []byte {
	h := sm3.New()
	h.Write(b)
	return h.Sum(nil)
}

func hmacSM3(key, data []byte) []byte {
	m := hmac.New(sm3.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// Seal 加密并认证明文，返回 IV|密文|MAC。
func Seal(key, plain []byte) ([]byte, error) {
	encKey, macKey := DeriveKeys(key)
	block, err := sm4.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	iv := make([]byte, IVLen)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	padded := pkcs7Pad(plain, block.BlockSize())
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	out := make([]byte, 0, IVLen+len(ct)+MACLen)
	out = append(out, iv...)
	out = append(out, ct...)
	out = append(out, hmacSM3(macKey, out)...)
	return out, nil
}

// Open 校验并解密 Seal 产生的报文。
func Open(key, data []byte) ([]byte, error) {
	if len(data) < IVLen+16+MACLen {
		return nil, ErrBadEnvelope
	}
	encKey, macKey := DeriveKeys(key)
	body := data[:len(data)-MACLen]
	want := data[len(data)-MACLen:]
	if subtle.ConstantTimeCompare(hmacSM3(macKey, body), want) != 1 {
		return nil, ErrBadMAC
	}
	block, err := sm4.NewCipher(encKey)
	if err != nil {
		return nil, err
	}
	iv := body[:IVLen]
	ct := body[IVLen:]
	if len(ct)%block.BlockSize() != 0 {
		return nil, ErrBadEnvelope
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	return pkcs7Unpad(pt, block.BlockSize())
}

func pkcs7Pad(b []byte, size int) []byte {
	n := size - len(b)%size
	out := make([]byte, len(b)+n)
	copy(out, b)
	for i := len(b); i < len(out); i++ {
		out[i] = byte(n)
	}
	return out
}

func pkcs7Unpad(b []byte, size int) ([]byte, error) {
	if len(b) == 0 || len(b)%size != 0 {
		return nil, ErrBadPadding
	}
	n := int(b[len(b)-1])
	if n == 0 || n > size || n > len(b) {
		return nil, ErrBadPadding
	}
	for _, c := range b[len(b)-n:] {
		if int(c) != n {
			return nil, ErrBadPadding
		}
	}
	return b[:len(b)-n], nil
}

// KeyPair 是一次握手中服务端持有的临时 SM2 密钥对。
type KeyPair struct {
	priv *sm2.PrivateKey
}

func NewKeyPair() (*KeyPair, error) {
	priv, err := sm2.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &KeyPair{priv: priv}, nil
}

// PublicHex 返回未压缩公钥：04 || X(32) || Y(32)，共 130 个 hex 字符。
func (k *KeyPair) PublicHex() string {
	buf := make([]byte, 65)
	buf[0] = 0x04
	x := k.priv.PublicKey.X.Bytes()
	y := k.priv.PublicKey.Y.Bytes()
	copy(buf[1+32-len(x):33], x)
	copy(buf[33+32-len(y):65], y)
	return hex.EncodeToString(buf)
}

// Decrypt 解密客户端提交的 SM2 密文（04 || C1C3C2）。
func (k *KeyPair) Decrypt(cipherHex string) ([]byte, error) {
	raw, err := hex.DecodeString(cipherHex)
	if err != nil {
		return nil, fmt.Errorf("SM2 密文不是合法 hex：%w", err)
	}
	if len(raw) < 97 || raw[0] != 0x04 {
		return nil, errors.New("SM2 密文缺少 04 前缀或长度不足")
	}
	pt, err := sm2.Decrypt(k.priv, raw, sm2.C1C3C2)
	if err != nil {
		return nil, fmt.Errorf("SM2 解密失败：%w", err)
	}
	return pt, nil
}

// Encrypt SM2 加密（仅用于测试与工具链，服务端不调用）。
func (k *KeyPair) Encrypt(plain []byte) (string, error) {
	ct, err := sm2.Encrypt(&k.priv.PublicKey, plain, rand.Reader, sm2.C1C3C2)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(ct), nil
}

// ParsePublic 供测试使用：从 hex 还原公钥。
func ParsePublic(h string) (*sm2.PublicKey, error) {
	raw, err := hex.DecodeString(h)
	if err != nil {
		return nil, err
	}
	if len(raw) != 65 || raw[0] != 0x04 {
		return nil, errors.New("公钥格式错误")
	}
	curve := sm2.P256Sm2()
	return &sm2.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(raw[1:33]),
		Y:     new(big.Int).SetBytes(raw[33:65]),
	}, nil
}

// B64 / UnB64 便捷封装。
func B64(b []byte) string            { return base64.StdEncoding.EncodeToString(b) }
func UnB64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// RandomHex 生成 n 字节随机数的 hex 表示。
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("系统随机数不可用：" + err.Error())
	}
	return hex.EncodeToString(b)
}

// RandomKey 生成会话密钥。
func RandomKey() []byte {
	b := make([]byte, KeyLen)
	if _, err := rand.Read(b); err != nil {
		panic("系统随机数不可用：" + err.Error())
	}
	return b
}
