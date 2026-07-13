// dsticketkeys — DSTicket(方案 B)RSA 密钥对与公钥 JWKS 生成工具。
//
// 用途(decision-revisit-player-jwt-key-rotation.md §7.4):
//   - 首次从共享 HS256 迁移到方案 B 时生成 K1(一次性;之后普通发布只换镜像 digest,
//     永不动密钥);
//   - 罕见的安全轮换时生成 K2,并用 -merge 把旧 JWKS 的公钥并进新 keyset(重叠窗口)；
//   - 后续阶段用 -private-in 复用同一 K2 私钥，-active-kid 独立控制 JWKS active key。
//
// 输出:
//
//	<out>/private.pem   RSA-2048 私钥(PKCS#8)。只进 K8s Secret(0400),绝不进
//	                    ConfigMap / 命令行参数 / 明文环境变量 / 版本库。
//	<out>/jwks.json     公钥 keyset(kid=RFC 7638 指纹,revision 由 -revision 指定)。
//	                    进不可变 ConfigMap pandora-dsticket-jwks-r<revision>。
//
// 示例:
//
//	go run ./tools/dsticketkeys -out ./run/dev/dsticket -revision 1
//	go run ./tools/dsticketkeys -out ./run/dev/dsticket-r2 -revision 2 -merge ./run/dev/dsticket/jwks.json -active-kid <K1-kid>
//	go run ./tools/dsticketkeys -out ./run/dev/dsticket-r3 -revision 3 -private-in ./run/dev/dsticket-r2/private.pem -merge ./run/dev/dsticket-r2/jwks.json
//	go run ./tools/dsticketkeys -out ./run/dev/dsticket-r4 -revision 4 -private-in ./run/dev/dsticket-r2/private.pem
package main

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/luyuancpp/pandora/pkg/auth"
)

func main() {
	outDir := flag.String("out", "", "输出目录(必填;已存在 private.pem/jwks.json 时拒绝覆盖)")
	revision := flag.Int("revision", 1, "keyset revision(与 ConfigMap 名 pandora-dsticket-jwks-<revision> 对应)")
	merge := flag.String("merge", "", "可选:旧 jwks.json 路径,把旧公钥并入新 keyset(轮换重叠窗口)")
	privateIn := flag.String("private-in", "", "可选:复用既有 RSA 私钥 PEM(用于提升 active 或生成 K2-only 退役版本；不生成新密钥)")
	activeKidFlag := flag.String("active-kid", "", "可选:JWKS 顶层 active_kid；默认取本次私钥 kid，Phase A 可显式保持旧 K1")
	flag.Parse()

	if *outDir == "" {
		fatalf("必须指定 -out 输出目录")
	}
	if *revision < 1 {
		fatalf("-revision 必须 >= 1")
	}
	privPath := filepath.Join(*outDir, "private.pem")
	jwksPath := filepath.Join(*outDir, "jwks.json")
	for _, p := range []string{privPath, jwksPath} {
		if _, err := os.Stat(p); err == nil {
			fatalf("拒绝覆盖已存在文件 %s(密钥生成是一次性操作;确需重生成请手工移走旧文件)", p)
		}
	}

	var privPEM []byte
	var pub *rsa.PublicKey
	var kid string
	var err error
	if *privateIn == "" {
		privPEM, pub, kid, err = auth.GenerateDSTicketKeyPair()
		if err != nil {
			fatalf("生成密钥对失败: %v", err)
		}
	} else {
		privPEM, pub, kid, err = loadPrivateKey(*privateIn)
		if err != nil {
			fatalf("复用既有私钥失败: %v", err)
		}
	}

	pubs := []*rsa.PublicKey{pub}
	if *merge != "" {
		data, err := os.ReadFile(*merge)
		if err != nil {
			fatalf("读取旧 JWKS 失败: %v", err)
		}
		oldKeys, err := auth.ParseDSTicketJWKS(data)
		if err != nil {
			fatalf("旧 JWKS 不合规,拒绝合并: %v", err)
		}
		// 按 kid 排序合并,输出确定性。
		kids := make([]string, 0, len(oldKeys))
		for k := range oldKeys {
			kids = append(kids, k)
		}
		sort.Strings(kids)
		for _, k := range kids {
			if k == kid {
				continue
			}
			pubs = append(pubs, oldKeys[k])
		}
	}

	activeKid := *activeKidFlag
	if activeKid == "" {
		activeKid = kid
	}
	jwks, err := auth.MarshalDSTicketJWKS(*revision, activeKid, pubs...)
	if err != nil {
		fatalf("编码 JWKS 失败: %v", err)
	}
	// 自校验:输出必须能被严格解析器接受(投递前最后一道闸)。
	if _, err := auth.ParseDSTicketJWKS(jwks); err != nil {
		fatalf("自校验失败(bug): %v", err)
	}

	if err := os.MkdirAll(*outDir, 0o700); err != nil {
		fatalf("创建输出目录失败: %v", err)
	}
	if err := os.WriteFile(privPath, privPEM, 0o600); err != nil {
		fatalf("写私钥失败: %v", err)
	}
	if err := os.WriteFile(jwksPath, append(jwks, '\n'), 0o644); err != nil {
		fatalf("写 JWKS 失败: %v", err)
	}

	fmt.Printf("已生成 DSTicket 阶段材料:\n")
	fmt.Printf("  私钥: %s (只进 K8s Secret,勿入版本库/ConfigMap)\n", privPath)
	fmt.Printf("  JWKS: %s (revision=%d, active_kid=%s, keys=%d)\n", jwksPath, *revision, activeKid, len(pubs))
	fmt.Printf("  私钥 kid: %s\n", kid)
}

func loadPrivateKey(path string) ([]byte, *rsa.PublicKey, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, "", fmt.Errorf("读取 %s: %w", path, err)
	}
	block, rest := pem.Decode(data)
	if block == nil || len(rest) != 0 {
		return nil, nil, "", fmt.Errorf("%s 不是单一 canonical PEM block", path)
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, nil, "", fmt.Errorf("解析 PKCS#8: %w", err)
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, nil, "", fmt.Errorf("PKCS#8 私钥不是 RSA")
		}
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, nil, "", fmt.Errorf("解析 PKCS#1: %w", err)
		}
	default:
		return nil, nil, "", fmt.Errorf("不支持 PEM 类型 %q", block.Type)
	}
	if err := key.Validate(); err != nil {
		return nil, nil, "", fmt.Errorf("RSA 私钥校验失败: %w", err)
	}
	if key.N.BitLen() < auth.DSTicketMinRSABits {
		return nil, nil, "", fmt.Errorf("RSA 私钥仅 %d bits，至少需要 %d", key.N.BitLen(), auth.DSTicketMinRSABits)
	}
	return data, &key.PublicKey, auth.RSAPublicKeyThumbprint(&key.PublicKey), nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "dsticketkeys: "+format+"\n", args...)
	os.Exit(1)
}
