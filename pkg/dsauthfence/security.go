package dsauthfence

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var etcdIdentityRevisionPattern = regexp.MustCompile(`^r[1-9][0-9]*$`)

const (
	EnvEtcdRequireMTLS         = "PANDORA_DS_AUTH_ETCD_REQUIRE_MTLS"
	EnvEtcdCAFile              = "PANDORA_DS_AUTH_ETCD_CA_FILE"
	EnvEtcdCertFile            = "PANDORA_DS_AUTH_ETCD_CERT_FILE"
	EnvEtcdKeyFile             = "PANDORA_DS_AUTH_ETCD_KEY_FILE"
	EnvEtcdServerName          = "PANDORA_DS_AUTH_ETCD_SERVER_NAME"
	EnvEtcdClientIdentity      = "PANDORA_DS_AUTH_ETCD_CLIENT_IDENTITY"
	EnvEtcdIdentityRevision    = "PANDORA_DS_AUTH_ETCD_IDENTITY_REVISION"
	EnvEtcdUsernameFile        = "PANDORA_DS_AUTH_ETCD_USERNAME_FILE"
	EnvEtcdPasswordFile        = "PANDORA_DS_AUTH_ETCD_PASSWORD_FILE"
	EnvEtcdRequireAuth         = "PANDORA_DS_AUTH_ETCD_REQUIRE_AUTH"
	EnvEtcdForbiddenReadPrefix = "PANDORA_DS_AUTH_ETCD_FORBIDDEN_READ_PREFIX"
)

// ClientSecurity 描述 etcd 客户端的生产传输与最小权限证明。Username/Password
// 是可选的：部署可以使用 etcd 基于客户端证书 CN 的身份，也可以叠加 v3 用户认证。
// RequireAuth 只要求服务端 auth 已启用，不强迫两种身份机制同时存在。
type ClientSecurity struct {
	RequireMTLS         bool
	CAFile              string
	CertFile            string
	KeyFile             string
	ServerName          string
	ClientIdentity      string
	IdentityRevision    string
	UsernameFile        string
	PasswordFile        string
	RequireAuth         bool
	ForbiddenReadPrefix string
}

func (s ClientSecurity) enabled() bool {
	return s.RequireMTLS || s.RequireAuth || s.CAFile != "" || s.CertFile != "" || s.KeyFile != "" ||
		s.ServerName != "" || s.ClientIdentity != "" || s.IdentityRevision != "" ||
		s.UsernameFile != "" || s.PasswordFile != "" || s.ForbiddenReadPrefix != ""
}

// ClientSecurityFromEnv 只读取路径/开关，不读取或回显凭据内容。生产 Pod 由
// revisioned immutable Secret 将这些路径注入；本地无这些环境变量时保持旧明文开发路径。
func ClientSecurityFromEnv() (ClientSecurity, error) {
	requireMTLS, err := strictBoolEnv(EnvEtcdRequireMTLS)
	if err != nil {
		return ClientSecurity{}, err
	}
	requireAuth, err := strictBoolEnv(EnvEtcdRequireAuth)
	if err != nil {
		return ClientSecurity{}, err
	}
	return ClientSecurity{
		RequireMTLS:         requireMTLS,
		CAFile:              strings.TrimSpace(os.Getenv(EnvEtcdCAFile)),
		CertFile:            strings.TrimSpace(os.Getenv(EnvEtcdCertFile)),
		KeyFile:             strings.TrimSpace(os.Getenv(EnvEtcdKeyFile)),
		ServerName:          strings.TrimSpace(os.Getenv(EnvEtcdServerName)),
		ClientIdentity:      strings.TrimSpace(os.Getenv(EnvEtcdClientIdentity)),
		IdentityRevision:    strings.TrimSpace(os.Getenv(EnvEtcdIdentityRevision)),
		UsernameFile:        strings.TrimSpace(os.Getenv(EnvEtcdUsernameFile)),
		PasswordFile:        strings.TrimSpace(os.Getenv(EnvEtcdPasswordFile)),
		RequireAuth:         requireAuth,
		ForbiddenReadPrefix: strings.TrimSpace(os.Getenv(EnvEtcdForbiddenReadPrefix)),
	}, nil
}

func strictBoolEnv(name string) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	switch value {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("dsauthfence: %s must be empty, 0, or 1", name)
	}
}

// DialSecureEtcdClient 以与 AcquireRuntime 完全相同的生产安全姿态(mTLS/最小权限
// 证明均从环境读取)连接 etcd。供同 module 的栅栏子包(writerlease 继任租约)复用,
// 保证所有 DS 授权体系的 etcd 客户端走同一套安全构造,不允许旁路明文路径分叉。
func DialSecureEtcdClient(endpoints []string, timeout time.Duration, prefix string) (*clientv3.Client, error) {
	security, err := ClientSecurityFromEnv()
	if err != nil {
		return nil, err
	}
	return newEtcdClient(endpoints, timeout, prefix, security)
}

func newEtcdClient(endpoints []string, timeout time.Duration, prefix string, security ClientSecurity) (*clientv3.Client, error) {
	config := clientv3.Config{Endpoints: endpoints, DialTimeout: timeout}
	if security.enabled() {
		if err := validateClientSecurity(endpoints, prefix, security); err != nil {
			return nil, err
		}
		tlsConfig, err := loadTLSConfig(security)
		if err != nil {
			return nil, err
		}
		config.TLS = tlsConfig
		if security.UsernameFile != "" {
			config.Username, err = readCredentialFile(security.UsernameFile, "username")
			if err != nil {
				return nil, err
			}
			config.Password, err = readCredentialFile(security.PasswordFile, "password")
			if err != nil {
				return nil, err
			}
		}
	}
	cli, err := clientv3.New(config)
	if err != nil {
		return nil, fmt.Errorf("dsauthfence: dial secure etcd: %w", err)
	}
	if !security.enabled() {
		return cli, nil
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := verifyClientSecurity(probeCtx, cli, security); err != nil {
		_ = cli.Close()
		return nil, err
	}
	return cli, nil
}

func validateClientSecurity(endpoints []string, allowedPrefix string, security ClientSecurity) error {
	if !security.RequireMTLS {
		return errors.New("dsauthfence: secure etcd configuration requires mTLS")
	}
	for name, value := range map[string]string{
		"custom CA file": security.CAFile, "client certificate file": security.CertFile,
		"client key file": security.KeyFile, "server name": security.ServerName,
		"client certificate identity": security.ClientIdentity, "identity revision": security.IdentityRevision,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("dsauthfence: missing %s", name)
		}
	}
	if !etcdIdentityRevisionPattern.MatchString(security.IdentityRevision) {
		return errors.New("dsauthfence: etcd identity revision must be canonical rN")
	}
	if (security.UsernameFile == "") != (security.PasswordFile == "") {
		return errors.New("dsauthfence: username/password files must be configured together")
	}
	if security.RequireAuth && security.ForbiddenReadPrefix == "" {
		return errors.New("dsauthfence: auth proof requires a forbidden read prefix")
	}
	if security.ForbiddenReadPrefix != "" {
		allowed := cleanPrefix(allowedPrefix)
		forbidden := cleanPrefix(security.ForbiddenReadPrefix)
		if allowed == forbidden || strings.HasPrefix(allowed, forbidden) || strings.HasPrefix(forbidden, allowed) {
			return errors.New("dsauthfence: forbidden read prefix overlaps the allowed DS auth prefix")
		}
	}
	for _, endpoint := range endpoints {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Port() == "" || parsed.User != nil ||
			parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("dsauthfence: production etcd endpoint must be canonical https://host:port")
		}
		port, portErr := strconv.ParseUint(parsed.Port(), 10, 16)
		if portErr != nil || port == 0 || strconv.FormatUint(port, 10) != parsed.Port() {
			return fmt.Errorf("dsauthfence: production etcd endpoint must use a canonical TCP port in 1..65535")
		}
	}
	return nil
}

func loadTLSConfig(security ClientSecurity) (*tls.Config, error) {
	caPEM, err := os.ReadFile(filepath.Clean(security.CAFile))
	if err != nil {
		return nil, fmt.Errorf("dsauthfence: read custom CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("dsauthfence: custom CA contains no certificates")
	}
	certificate, err := tls.LoadX509KeyPair(filepath.Clean(security.CertFile), filepath.Clean(security.KeyFile))
	if err != nil {
		return nil, fmt.Errorf("dsauthfence: load client certificate/key: %w", err)
	}
	if len(certificate.Certificate) == 0 {
		return nil, errors.New("dsauthfence: client certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("dsauthfence: parse client certificate: %w", err)
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("dsauthfence: client certificate is not currently valid")
	}
	if leaf.Subject.CommonName != security.ClientIdentity {
		return nil, errors.New("dsauthfence: client certificate CN does not match configured identity")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      roots,
		Certificates: []tls.Certificate{certificate},
		ServerName:   security.ServerName,
	}, nil
}

func readCredentialFile(path, kind string) (string, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("dsauthfence: read etcd %s file: %w", kind, err)
	}
	value := string(raw)
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("dsauthfence: etcd %s file is empty or non-canonical", kind)
	}
	return value, nil
}

func verifyClientSecurity(ctx context.Context, cli *clientv3.Client, security ClientSecurity) error {
	statusResponse, err := cli.AuthStatus(ctx)
	if err != nil {
		return fmt.Errorf("dsauthfence: etcd auth status probe failed: %w", err)
	}
	if security.RequireAuth && !statusResponse.Enabled {
		return errors.New("dsauthfence: etcd auth is disabled")
	}
	if security.ForbiddenReadPrefix == "" {
		return nil
	}
	_, err = cli.Get(ctx, cleanPrefix(security.ForbiddenReadPrefix), clientv3.WithPrefix(), clientv3.WithLimit(1))
	if status.Code(err) != codes.PermissionDenied {
		if err == nil {
			return errors.New("dsauthfence: ACL negative probe unexpectedly succeeded")
		}
		return fmt.Errorf("dsauthfence: ACL negative probe did not return permission denied: %w", err)
	}
	return nil
}
