package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luyuancpp/pandora/pkg/auth"
)

func TestLoadPrivateKeyRoundTrip(t *testing.T) {
	privatePEM, wantPublic, wantKid, err := auth.GenerateDSTicketKeyPair()
	if err != nil {
		t.Fatalf("GenerateDSTicketKeyPair: %v", err)
	}
	path := filepath.Join(t.TempDir(), "private.pem")
	if err := os.WriteFile(path, privatePEM, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	gotPEM, gotPublic, gotKid, err := loadPrivateKey(path)
	if err != nil {
		t.Fatalf("loadPrivateKey: %v", err)
	}
	if !bytes.Equal(gotPEM, privatePEM) {
		t.Fatal("复用私钥时必须原样保留 canonical PEM bytes")
	}
	if gotKid != wantKid || gotPublic.N.Cmp(wantPublic.N) != 0 || gotPublic.E != wantPublic.E {
		t.Fatalf("复用私钥的公钥参数漂移:kid=%q want=%q", gotKid, wantKid)
	}
}

func TestLoadPrivateKeyAcceptsPKCS1(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, auth.DSTicketMinRSABits)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	path := filepath.Join(t.TempDir(), "private-pkcs1.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, public, kid, err := loadPrivateKey(path)
	if err != nil {
		t.Fatalf("loadPrivateKey(PKCS#1): %v", err)
	}
	if public.N.Cmp(key.N) != 0 || kid != auth.RSAPublicKeyThumbprint(&key.PublicKey) {
		t.Fatal("PKCS#1 复用后公钥或 kid 漂移")
	}
}

func TestLoadPrivateKeyRejectsExtraPEMDataAndWeakKey(t *testing.T) {
	privatePEM, _, _, err := auth.GenerateDSTicketKeyPair()
	if err != nil {
		t.Fatalf("GenerateDSTicketKeyPair: %v", err)
	}
	extraPath := filepath.Join(t.TempDir(), "extra.pem")
	if err := os.WriteFile(extraPath, append(append([]byte{}, privatePEM...), '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile(extra): %v", err)
	}
	if _, _, _, err := loadPrivateKey(extraPath); err == nil || !strings.Contains(err.Error(), "单一 canonical PEM") {
		t.Fatalf("额外 PEM 数据必须被拒绝，得到:%v", err)
	}

	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("GenerateKey(weak): %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(weak)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	weakPath := filepath.Join(t.TempDir(), "weak.pem")
	if err := os.WriteFile(weakPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("WriteFile(weak): %v", err)
	}
	if _, _, _, err := loadPrivateKey(weakPath); err == nil || !strings.Contains(err.Error(), "至少需要") {
		t.Fatalf("弱 RSA 私钥必须被拒绝，得到:%v", err)
	}
}
