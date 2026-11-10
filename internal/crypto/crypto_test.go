package crypto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	key := RandomKey()
	for _, n := range []int{0, 1, 15, 16, 17, 1000, 70000} {
		plain := bytes.Repeat([]byte("中文abcdef"), n/10+1)[:n]
		sealed, err := Seal(key, plain)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		got, err := Open(key, sealed)
		if err != nil {
			t.Fatalf("open(n=%d): %v", n, err)
		}
		if !bytes.Equal(got, plain) {
			t.Fatalf("往返数据不一致 n=%d", n)
		}
	}
}

func TestSealIsRandomized(t *testing.T) {
	key := RandomKey()
	a, _ := Seal(key, []byte("same"))
	b, _ := Seal(key, []byte("same"))
	if bytes.Equal(a, b) {
		t.Fatal("相同明文两次加密结果相同，说明 IV 未随机化")
	}
}

func TestTamperDetected(t *testing.T) {
	key := RandomKey()
	sealed, _ := Seal(key, []byte("hello"))
	sealed[20] ^= 0xff
	if _, err := Open(key, sealed); err == nil {
		t.Fatal("篡改后仍然解密成功")
	}
	other := RandomKey()
	sealed2, _ := Seal(key, []byte("hello"))
	if _, err := Open(other, sealed2); err == nil {
		t.Fatal("错误密钥仍然解密成功")
	}
}

// TestInteropWithJSSM2 用 Node + sm-crypto 生成密文，验证 Go 端能正确解密。
// 需要本机有 node 与 sm-crypto 模块，通过 SMCRYPTO_PATH 指定模块目录，否则跳过。
func TestInteropWithJSSM2(t *testing.T) {
	modPath := os.Getenv("SMCRYPTO_PATH")
	if modPath == "" {
		t.Skip("未设置 SMCRYPTO_PATH，跳过跨语言校验")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("未安装 node，跳过跨语言校验")
	}
	kp, err := NewKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	pub := kp.PublicHex()

	script := filepath.Join(t.TempDir(), "enc.js")
	code := `
const sm2 = require(process.argv[2]);
const pub = process.argv[3];
const keyHex = process.argv[4];
const bytes = [];
for (let i = 0; i < keyHex.length; i += 2) bytes.push(parseInt(keyHex.substr(i, 2), 16));
let ct = sm2.doEncrypt(bytes, pub, 1);
if (!ct.startsWith('04')) ct = '04' + ct;
process.stdout.write(ct);
`
	if err := os.WriteFile(script, []byte(code), 0o644); err != nil {
		t.Fatal(err)
	}
	keyHex := hex.EncodeToString(RandomKey())
	out, err := exec.Command("node", script, modPath, pub, keyHex).Output()
	if err != nil {
		t.Fatalf("node 执行失败：%v", err)
	}
	got, err := kp.Decrypt(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("Go 解密 JS 密文失败：%v", err)
	}
	if hex.EncodeToString(got) != keyHex {
		t.Fatalf("密钥不一致：want %s got %s", keyHex, hex.EncodeToString(got))
	}
}

// TestInteropEnvelopeWithJS 双向校验 SM4-CBC + HMAC-SM3 信封与浏览器实现一致。
func TestInteropEnvelopeWithJS(t *testing.T) {
	dir := os.Getenv("SMCRYPTO_DIR")
	if dir == "" {
		t.Skip("未设置 SMCRYPTO_DIR，跳过信封跨语言校验")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("未安装 node")
	}
	run := func(mode, arg string) string {
		cmd := exec.Command("node", "test/interop.mjs", mode, arg)
		cmd.Dir = repoRoot(t)
		cmd.Env = append(os.Environ(), "SMCRYPTO_DIR="+dir)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("node %s 失败：%v", mode, err)
		}
		return string(out)
	}

	keyHex := "000102030405060708090a0b0c0d0e0f"
	key, _ := hex.DecodeString(keyHex)
	plain := "echo 你好，shell-go；line2\n"

	// JS 加密 -> Go 解密
	sealedB64 := run("seal", `{"key":"`+keyHex+`","plain":`+jsonString(plain)+`}`)
	sealed, err := UnB64(sealedB64)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(key, sealed)
	if err != nil {
		t.Fatalf("Go 解密 JS 报文失败：%v", err)
	}
	if string(got) != plain {
		t.Fatalf("明文不一致：%q != %q", got, plain)
	}

	// Go 加密 -> JS 解密
	sealed2, err := Seal(key, []byte(plain))
	if err != nil {
		t.Fatal(err)
	}
	out := run("open", `{"key":"`+keyHex+`","data":"`+B64(sealed2)+`"}`)
	if out != plain {
		t.Fatalf("JS 解密 Go 报文不一致：%q != %q", out, plain)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		wd = filepath.Dir(wd)
	}
	t.Fatal("找不到项目根目录")
	return ""
}
