package framework

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	mrand "math/rand"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golang-jwt/jwt"
	"github.com/google/go-cmp/cmp"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	"golang.org/x/xerrors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"sigs.k8s.io/yaml"

	"go.f110.dev/heimdallr/pkg/auth/authn"
	"go.f110.dev/heimdallr/pkg/authproxy"
	"go.f110.dev/heimdallr/pkg/cert"
	"go.f110.dev/heimdallr/pkg/cert/vault"
	"go.f110.dev/heimdallr/pkg/config/configv2"
	"go.f110.dev/heimdallr/pkg/database"
	"go.f110.dev/heimdallr/pkg/netutil"
	"go.f110.dev/heimdallr/pkg/rpc"
	"go.f110.dev/heimdallr/pkg/rpc/rpcclient"
	"go.f110.dev/heimdallr/pkg/session"
	"go.f110.dev/heimdallr/pkg/testing/btesting"
)

const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

const (
	RootUserId    = "root@e2e.f110.dev"
	vaultRoleName = "heimdallr"
)

var (
	binaryPath            *string
	connectorBinaryPath   *string
	tunnelBinaryPath      *string
	vaultLatestBinaryPath *string
	vaultV110BinaryPath   *string
)

func init() {
	binaryPath = flag.String("e2e.binary", "", "")
	connectorBinaryPath = flag.String("e2e.connector-binary", "", "")
	tunnelBinaryPath = flag.String("e2e.tunnel-binary", "", "")
	vaultLatestBinaryPath = flag.String("e2e.vault-binary", "", "")
	vaultV110BinaryPath = flag.String("e2e.vault_110-binary", "", "")
}

type mockServer interface {
	Start() error
	Stop() error
}

type Connector struct {
	name   string
	target *btesting.MockServer

	dir        string
	csr        []byte
	privateKey crypto.PrivateKey
	cmd        *exec.Cmd
	running    bool
}

func NewConnector(name string, m *btesting.MockServer, proxyCACert *x509.Certificate) (*Connector, error) {
	dir, err := os.MkdirTemp("", "")
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}

	csr, privateKey, err := cert.CreatePrivateKeyAndCertificateRequest(pkix.Name{CommonName: name}, nil)
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	b, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}
	err = cert.PemEncode(filepath.Join(dir, "agent.key"), "EC PRIVATE KEY", b, nil)
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	err = cert.PemEncode(filepath.Join(dir, "ca.crt"), "CERTIFICATE", proxyCACert.Raw, nil)
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}

	return &Connector{name: name, target: m, privateKey: privateKey, csr: csr, dir: dir}, nil
}

func (c *Connector) Start(client *rpcclient.ClientWithUserToken, host, serverName string) error {
	if c.dir == "" {
		return xerrors.New("already ran and stopped")
	}

	cer, err := client.NewAgentCertByCSR(string(c.csr), c.name)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	err = cert.PemEncode(filepath.Join(c.dir, "agent.crt"), "CERTIFICATE", cer, nil)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}

	c.cmd = exec.Command(*connectorBinaryPath,
		fmt.Sprintf("--ca-cert=%s", filepath.Join(c.dir, "ca.crt")),
		fmt.Sprintf("--certificate=%s", filepath.Join(c.dir, "agent.crt")),
		fmt.Sprintf("--privatekey=%s", filepath.Join(c.dir, "agent.key")),
		fmt.Sprintf("--name=%s", c.name),
		fmt.Sprintf("--backend=localhost:%d", c.target.Port),
		fmt.Sprintf("--host=%s", host),
		fmt.Sprintf("--server-name=%s", serverName),
		"--debug",
	)
	if *verbose {
		c.cmd.Stdout = os.Stdout
		c.cmd.Stderr = os.Stderr
	}
	err = c.cmd.Start()
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	c.running = true
	time.Sleep(1 * time.Second)

	return nil
}

func (c *Connector) Stop() error {
	if err := os.RemoveAll(c.dir); err != nil {
		return xerrors.Errorf(": %w", err)
	}
	c.dir = ""

	doneCh := make(chan struct{})
	go func() {
		if c.cmd != nil && c.cmd.Process != nil {
			c.cmd.Process.Wait()
		}
		close(doneCh)
	}()
	if err := c.cmd.Process.Signal(os.Interrupt); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	select {
	case <-time.After(3 * time.Second):
		return c.cmd.Process.Signal(os.Kill)
	case <-doneCh:
	}

	return nil
}

type ProxyCond func(*Proxy)

func WithLatestVault(p *Proxy) {
	p.ca = "vault"
	p.vaultBinaryPath = *vaultLatestBinaryPath
}

func WithVaultV110(p *Proxy) {
	p.ca = "vault"
	p.vaultBinaryPath = *vaultV110BinaryPath
}

type Proxy struct {
	Domain     string
	DomainHost string
	CA         *x509.Certificate

	sessionStore *session.SecureCookieStore

	t                  *testing.T
	running            bool
	dir                string
	proxyPort          int
	internalPort       int
	rpcPort            int
	dashboardPort      int
	vaultPort          int
	internalToken      string
	ca                 string
	vaultCmd           *exec.Cmd
	vaultBinaryPath    string
	vaultAddr          string
	vaultRootToken     string
	caPrivateKey       crypto.PrivateKey
	signPrivateKey     *ecdsa.PrivateKey
	backends           []*configv2.Backend
	roles              []*configv2.Role
	rpcPermissions     []*configv2.RPCPermission
	users              []*database.User
	mockServers        []mockServer
	runningMockServers []mockServer
	connectors         []*Connector

	configBuf                []byte
	proxyConfBuf             []byte
	roleConfBuf              []byte
	rpcPermissionConfBuf     []byte
	prevConfigBuf            []byte
	prevProxyConfBuf         []byte
	prevRoleConfBuf          []byte
	prevRpcPermissionConfBuf []byte

	identityProvider *IdentityProvider
	proxyCmd         *exec.Cmd
	err              error
	rpcClient        *rpcclient.ClientWithUserToken
}

func NewProxy(t *testing.T, conds ...ProxyCond) (*Proxy, error) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "data"), 0700); err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}

	signReqKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	b, err := x509.MarshalECPrivateKey(signReqKey)
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}
	if err := cert.PemEncode(filepath.Join(dir, "privatekey.pem"), "EC PRIVATE KEY", b, nil); err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}
	b, err = x509.MarshalPKIXPublicKey(signReqKey.Public())
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	if err := cert.PemEncode(filepath.Join(dir, "publickey.pem"), "PUBLIC KEY", b, nil); err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	caCert, caPrivateKey, err := cert.CreateCertificateAuthority("heimdallr proxy e2e", "test", "e2e", "jp", "ecdsa")
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	b, err = x509.MarshalECPrivateKey(caPrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	if err := cert.PemEncode(filepath.Join(dir, "ca.crt"), "CERTIFICATE", caCert.Raw, nil); err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	if err := cert.PemEncode(filepath.Join(dir, "ca.key"), "EC PRIVATE KEY", b, nil); err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	internalToken := make([]byte, 32)
	for i := range internalToken {
		internalToken[i] = letters[mrand.Intn(len(letters))]
	}
	f, err := os.Create(filepath.Join(dir, "internal_token"))
	if err != nil {
		return nil, xerrors.Errorf(": %v", err)
	}
	f.Write(internalToken)
	f.Close()
	hashKey := session.GenerateRandomKey(32)
	blockKey := session.GenerateRandomKey(16)
	f, err = os.Create(filepath.Join(dir, "cookie_secret"))
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	f.WriteString(hex.EncodeToString(hashKey))
	f.WriteString("\n")
	f.WriteString(hex.EncodeToString(blockKey))
	f.Close()

	sessionStore, err := session.NewSecureCookieStore([]byte(hex.EncodeToString(hashKey)), []byte(hex.EncodeToString(blockKey)), "e2e.f110.dev")
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "identityprovider"), []byte("identityprovider"), 0644); err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}

	proxyPort, err := netutil.FindUnusedPort()
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	internalPort, err := netutil.FindUnusedPort()
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	rpcPort, err := netutil.FindUnusedPort()
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	dashboardPort, err := netutil.FindUnusedPort()
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}

	idp, err := NewIdentityProvider(fmt.Sprintf("https://e2e.f110.dev:%d/auth/callback", proxyPort))
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}

	// The vault server will use two ports.
	// One of the port is configurable via the command line argument.
	// The other port is not configurable.
	// The vault server listens to neighboring port on its own.
	// So, we can't use the neighboring port of the vault.
	vaultPort, err := netutil.FindUnusedPort()
	if err != nil {
		return nil, xerrors.Errorf(": %w", err)
	}
	vaultRootToken := make([]byte, 32)
	for i := range vaultRootToken {
		vaultRootToken[i] = letters[mrand.Intn(len(letters))]
	}

	p := &Proxy{
		Domain:           fmt.Sprintf("e2e.f110.dev:%d", proxyPort),
		DomainHost:       "e2e.f110.dev",
		CA:               caCert,
		sessionStore:     sessionStore,
		t:                t,
		dir:              dir,
		identityProvider: idp,
		signPrivateKey:   signReqKey,
		ca:               "local",
		caPrivateKey:     caPrivateKey,
		proxyPort:        proxyPort,
		internalPort:     internalPort,
		rpcPort:          rpcPort,
		dashboardPort:    dashboardPort,
		vaultPort:        vaultPort,
		vaultAddr:        fmt.Sprintf("http://127.0.0.1:%d", vaultPort),
		internalToken:    string(internalToken),
		vaultRootToken:   string(vaultRootToken),
	}
	for _, cond := range conds {
		cond(p)
	}

	return p, nil
}

func (p *Proxy) URL(subdomain string, pathAnd ...string) string {
	path := ""
	if len(pathAnd) > 0 {
		path = pathAnd[0]
	}
	if path == "" {
		return fmt.Sprintf("https://%s.%s/", subdomain, p.Domain)
	} else {
		u := &url.URL{
			Scheme: "https",
			Path:   path,
		}
		if subdomain == "" {
			u.Host = p.Domain
		} else {
			u.Host = subdomain + "." + p.Domain
		}
		return u.String()
	}
}

func (p *Proxy) Host(name string) string {
	return fmt.Sprintf("%s.%s", name, p.Domain)
}

func (p *Proxy) ProxyAddr() string {
	return fmt.Sprintf(":%d", p.proxyPort)
}

func (p *Proxy) Backend(b *configv2.Backend) {
	p.backends = append(p.backends, b)
}

func (p *Proxy) Role(r *configv2.Role) {
	p.roles = append(p.roles, r)
}

func (p *Proxy) RPCPermission(v *configv2.RPCPermission) {
	p.rpcPermissions = append(p.rpcPermissions, v)
}

func (p *Proxy) User(u *database.User) {
	p.users = append(p.users, u)
}

func (p *Proxy) MockServer() *btesting.MockServer {
	s, err := btesting.NewMockServer()
	if err != nil {
		return nil
	}
	p.mockServers = append(p.mockServers, s)

	return s
}

func (p *Proxy) MockTCPServer() *btesting.MockTCPServer {
	s, err := btesting.NewMockTCPServer()
	if err != nil {
		return nil
	}
	p.mockServers = append(p.mockServers, s)

	return s
}

func (p *Proxy) Connector(name string, m *btesting.MockServer) {
	c, err := NewConnector(name, m, p.CA)
	if err != nil {
		return
	}
	p.connectors = append(p.connectors, c)
}

func (p *Proxy) DashboardBackend() *configv2.Backend {
	return &configv2.Backend{
		Name:          "dashboard",
		AllowRootUser: true,
		HTTP: []*configv2.HTTPBackend{
			{Path: "/", Upstream: fmt.Sprintf("http://localhost:%d", p.dashboardPort)},
		},
		Permissions: []*configv2.Permission{
			{Name: "all", Locations: []configv2.Location{{Any: "/"}}},
		},
	}
}

func (p *Proxy) Cleanup() error {
	for _, v := range p.connectors {
		if err := v.Stop(); err != nil {
			return xerrors.Errorf(": %w", err)
		}
	}

	if err := p.stop(); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	for _, v := range p.runningMockServers {
		if err := v.Stop(); err != nil {
			return xerrors.Errorf(": %w", err)
		}
	}

	if p.vaultCmd != nil {
		p.vaultCmd.Process.Signal(syscall.SIGTERM)
	}

	return os.RemoveAll(p.dir)
}

func (p *Proxy) ClearConf() bool {
	p.backends = nil
	p.roles = nil
	p.rpcPermissions = nil
	p.users = nil
	p.mockServers = nil

	return true
}

func (p *Proxy) Reload() error {
	if err := p.buildConfig(); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	if changed, err := p.isChangedConfig(); err != nil {
		return xerrors.Errorf(": %w", err)
	} else if !changed {
		return nil
	}

	if *e2eDebug {
		log.Print("Start main process")
	}
	if p.running {
		for _, v := range p.runningMockServers {
			if err := v.Stop(); err != nil {
				return xerrors.Errorf(": %w", err)
			}
		}
		p.runningMockServers = nil

		if err := p.stop(); err != nil {
			return xerrors.Errorf(": %w", err)
		}
	}

	if p.ca == "vault" && p.vaultCmd == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.Command(
			p.vaultBinaryPath,
			"server",
			"-dev",
			fmt.Sprintf("-dev-listen-address=127.0.0.1:%d", p.vaultPort),
			fmt.Sprintf("-dev-root-token-id=%s", p.vaultRootToken),
		)
		cmd.Env = append(cmd.Env, fmt.Sprintf("HOME=%s", p.dir))
		if err := cmd.Start(); err != nil {
			return xerrors.Errorf(": %w", err)
		}
		p.vaultCmd = cmd
		if err := netutil.WaitListen(fmt.Sprintf(":%d", p.vaultPort), 10*time.Second); err != nil {
			return xerrors.Errorf(": %w", err)
		}

		vaultClient, err := vault.NewClient(fmt.Sprintf("http://127.0.0.1:%d", p.vaultPort), p.vaultRootToken, "pki", "")
		if err != nil {
			return xerrors.Errorf(": %w", err)
		}
		if err := vaultClient.EnablePKI(ctx); err != nil {
			return xerrors.Errorf(": %w", err)
		}
		err = vaultClient.SetRole(ctx, vaultRoleName, &vault.Role{
			AllowedDomains:   []string{rpc.ServerHostname},
			AllowSubDomains:  true,
			AllowLocalhost:   true,
			AllowBareDomains: true,
			AllowAnyName:     true,
			EnforceHostnames: false,
			ServerFlag:       true,
			ClientFlag:       true,
			KeyType:          "ec",
			KeyBits:          256,
		})
		if err != nil {
			return xerrors.Errorf(": %w", err)
		}
		if err := vaultClient.SetCA(ctx, p.CA, p.caPrivateKey); err != nil {
			return xerrors.Errorf(": %w", err)
		}
	}

	for _, v := range p.mockServers {
		if err := v.Start(); err != nil {
			return xerrors.Errorf(": %w", err)
		}
		p.runningMockServers = append(p.runningMockServers, v)
	}

	if err := p.start(); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	if err := p.setupRPCClient(); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	if err := p.syncUsers(); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	for _, v := range p.connectors {
		if *e2eDebug {
			log.Print("Start connector")
		}
		if err := v.Start(p.rpcClient, fmt.Sprintf("127.0.0.1:%d", p.proxyPort), p.DomainHost); err != nil {
			return xerrors.Errorf(": %w", err)
		}
	}

	return nil
}

func (p *Proxy) setupRPCClient() error {
	caPool, err := x509.SystemCertPool()
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	caPool.AddCert(p.CA)

	cred := credentials.NewTLS(&tls.Config{ServerName: rpc.ServerHostname, RootCAs: caPool})
	conn, err := grpc.Dial(
		fmt.Sprintf("127.0.0.1:%d", p.rpcPort),
		grpc.WithTransportCredentials(cred),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 20 * time.Second, Timeout: time.Second, PermitWithoutStream: true}),
		grpc.WithStreamInterceptor(retry.StreamClientInterceptor()),
		grpc.WithUnaryInterceptor(retry.UnaryClientInterceptor()),
	)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}

	claim := jwt.NewWithClaims(jwt.SigningMethodES256, &authn.TokenClaims{
		StandardClaims: jwt.StandardClaims{
			Id:        RootUserId,
			IssuedAt:  time.Now().Unix(),
			ExpiresAt: time.Now().Add(authproxy.TokenExpiration).Unix(),
		},
	})
	token, err := claim.SignedString(p.signPrivateKey)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}

	p.rpcClient = rpcclient.NewClientWithUserToken(conn).WithToken(token)
	return nil
}

func (p *Proxy) syncUsers() error {
	caPool, err := x509.SystemCertPool()
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	caPool.AddCert(p.CA)

	cred := credentials.NewTLS(&tls.Config{ServerName: rpc.ServerHostname, RootCAs: caPool})
	conn, err := grpc.Dial(
		fmt.Sprintf("127.0.0.1:%d", p.rpcPort),
		grpc.WithTransportCredentials(cred),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 20 * time.Second, Timeout: time.Second, PermitWithoutStream: true}),
		grpc.WithStreamInterceptor(retry.StreamClientInterceptor()),
		grpc.WithUnaryInterceptor(retry.UnaryClientInterceptor()),
	)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}

	claim := jwt.NewWithClaims(jwt.SigningMethodES256, &authn.TokenClaims{
		StandardClaims: jwt.StandardClaims{
			Id:        RootUserId,
			IssuedAt:  time.Now().Unix(),
			ExpiresAt: time.Now().Add(authproxy.TokenExpiration).Unix(),
		},
	})
	token, err := claim.SignedString(p.signPrivateKey)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}

	client := rpcclient.NewClientWithUserToken(conn).WithToken(token)
	users, err := client.ListAllUser()
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}

	for _, user := range users {
		if user.Id == database.SystemUser.Id {
			continue
		}
		for _, r := range user.Roles {
			if err := client.DeleteUser(user.Id, r); err != nil {
				return xerrors.Errorf(": %w", err)
			}
		}
	}

	for _, user := range p.users {
		for _, r := range user.Roles {
			if err := client.AddUser(user.Id, r); err != nil {
				return xerrors.Errorf(": %w", err)
			}
		}
	}

	return nil
}

func (p *Proxy) isChangedConfig() (bool, error) {
	if p.configBuf == nil || p.proxyConfBuf == nil || p.roleConfBuf == nil || p.rpcPermissionConfBuf == nil {
		return true, nil
	}

	if !bytes.Equal(p.prevConfigBuf, p.configBuf) {
		if *e2eDebug {
			log.Print("Changed config.yaml")
		}
		return true, nil
	}
	if !bytes.Equal(p.prevProxyConfBuf, p.proxyConfBuf) {
		if *e2eDebug {
			log.Print("Changed proxies.yaml")
			log.Print(cmp.Diff(string(p.prevProxyConfBuf), string(p.proxyConfBuf)))
		}
		return true, nil
	}
	if !bytes.Equal(p.prevRoleConfBuf, p.roleConfBuf) {
		if *e2eDebug {
			log.Print("Changed roles.yaml")
		}
		return true, nil
	}
	if !bytes.Equal(p.prevRpcPermissionConfBuf, p.rpcPermissionConfBuf) {
		if *e2eDebug {
			log.Print("Changed rpc_permissions.yaml")
		}
		return true, nil
	}

	return false, nil
}

func (p *Proxy) start() error {
	if err := p.setup(p.dir); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	if err := p.startProcess(); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	return p.waitForStart()
}

func (p *Proxy) startProcess() error {
	p.proxyCmd = exec.Command(*binaryPath, "-c", filepath.Join(p.dir, "config.yaml"))
	if *verbose {
		p.proxyCmd.Stdout = os.Stdout
		p.proxyCmd.Stderr = os.Stderr
	}
	err := p.proxyCmd.Start()
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	p.running = true
	//p.t.Logf("Start process :%d rpc :%d", p.proxyPort, p.rpcPort)

	return nil
}

func (p *Proxy) stop() error {
	var wg sync.WaitGroup
	if p.proxyCmd != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()

			p.proxyCmd.Process.Wait()
		}()

		if err := p.proxyCmd.Process.Signal(os.Interrupt); err != nil {
			return xerrors.Errorf(": %w", err)
		}
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		done <- struct{}{}
	}()

	select {
	case <-time.After(10 * time.Second):
		log.Print("stopping proxy process was timed out. We are going to send KILL signal to stop process forcibly.")
		return p.proxyCmd.Process.Signal(os.Kill)
	case <-done:
	}

	return nil
}

func (p *Proxy) waitForStart() error {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()

	timeout := time.After(10 * time.Second)
	for {
		select {
		case <-t.C:
			conn, err := net.Dial("tcp", fmt.Sprintf(":%d", p.proxyPort))
			if err != nil {
				continue
			}

			conn.Close()
			return nil
		case <-timeout:
			if p.running {
				_ = p.stop()
			}
			return xerrors.New("waiting for start process is timed out")
		}
	}
}

func (p *Proxy) setup(dir string) error {
	s := strings.SplitN(p.Domain, ":", 2)
	hosts := []string{s[0], "dashboard." + s[0]}
	for _, b := range p.backends {
		if b.FQDN != "" {
			hosts = append(hosts, b.FQDN)
			continue
		}
		if b.Name != "" {
			hosts = append(hosts, fmt.Sprintf("%s.%s", b.Name, s[0]))
			continue
		}
	}
	c, privateKey, err := cert.GenerateServerCertificate(p.CA, p.caPrivateKey, hosts)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	b, err := x509.MarshalECPrivateKey(privateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	if err := cert.PemEncode(filepath.Join(dir, "tls.key"), "EC PRIVATE KEY", b, nil); err != nil {
		return xerrors.Errorf(": %w", err)
	}
	if err := cert.PemEncode(filepath.Join(dir, "tls.crt"), "CERTIFICATE", c.Raw, nil); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	return nil
}

func (p *Proxy) buildConfig() error {
	p.prevConfigBuf = p.configBuf
	p.prevProxyConfBuf = p.proxyConfBuf
	p.prevRoleConfBuf = p.roleConfBuf
	p.prevRpcPermissionConfBuf = p.rpcPermissionConfBuf

	proxy := p.backends
	foundDashboard := false
	for _, v := range p.backends {
		if v.Name == "dashboard" {
			foundDashboard = true
		}
	}
	if !foundDashboard {
		proxy = append(proxy, p.DashboardBackend())
	}
	b, err := yaml.Marshal(proxy)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	p.proxyConfBuf = b
	if err := os.WriteFile(filepath.Join(p.dir, "proxies.yaml"), b, 0644); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	role := p.roles
	b, err = yaml.Marshal(role)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	p.roleConfBuf = b
	if err := os.WriteFile(filepath.Join(p.dir, "roles.yaml"), b, 0644); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	rpcPermission := p.rpcPermissions
	b, err = yaml.Marshal(rpcPermission)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	p.rpcPermissionConfBuf = b
	if err := os.WriteFile(filepath.Join(p.dir, "rpc_permissions.yaml"), b, 0644); err != nil {
		return xerrors.Errorf(": %w", err)
	}

	conf := &configv2.Config{
		AccessProxy: &configv2.AccessProxy{
			HTTP: &configv2.AuthProxyHTTP{
				ServerName:      fmt.Sprintf("e2e.f110.dev:%d", p.proxyPort),
				Bind:            fmt.Sprintf(":%d", p.proxyPort),
				BindInternalApi: fmt.Sprintf(":%d", p.internalPort),
				Certificate: &configv2.Certificate{
					CertFile: "./tls.crt",
					KeyFile:  "./tls.key",
				},
				Session: &configv2.Session{
					Type:    "secure_cookie",
					KeyFile: "./cookie_secret",
				},
			},
			Credential: &configv2.Credential{
				SigningPrivateKeyFile: "./privatekey.pem",
				InternalTokenFile:     "./internal_token",
			},
			RPCServer: fmt.Sprintf("127.0.0.1:%d", p.rpcPort),
			ProxyFile: "./proxies.yaml",
		},
		CertificateAuthority: &configv2.CertificateAuthority{},
		AuthorizationEngine: &configv2.AuthorizationEngine{
			RoleFile:          "./roles.yaml",
			RPCPermissionFile: "./rpc_permissions.yaml",
			RootUsers:         []string{RootUserId},
		},
		Logger: &configv2.Logger{
			Encoding: "console",
			Level:    "debug",
		},
		RPCServer: &configv2.RPCServer{
			Bind: fmt.Sprintf(":%d", p.rpcPort),
		},
		IdentityProvider: &configv2.IdentityProvider{
			Provider:         "custom",
			Issuer:           p.identityProvider.Issuer,
			ClientId:         "e2e",
			ClientSecretFile: "./identityprovider",
			RedirectUrl:      fmt.Sprintf("https://e2e.f110.dev:%d/auth/callback", p.proxyPort),
			ExtraScopes:      []string{"email"},
		},
		Datastore: &configv2.Datastore{
			DatastoreEtcd: &configv2.DatastoreEtcd{
				RawUrl:  "etcd://embed",
				DataDir: "./data",
			},
		},
		Dashboard: &configv2.Dashboard{
			Bind:         fmt.Sprintf(":%d", p.dashboardPort),
			PublicKeyUrl: fmt.Sprintf("http://:%d/internal/publickey", p.internalPort),
		},
	}

	switch p.ca {
	case "local":
		conf.CertificateAuthority.Local = &configv2.CertificateAuthorityLocal{
			CertFile:         "./ca.crt",
			KeyFile:          "./ca.key",
			Organization:     "test",
			OrganizationUnit: "e2e",
			Country:          "jp",
		}
	case "vault":
		conf.CertificateAuthority.Vault = &configv2.CertificateAuthorityVault{
			Addr:  p.vaultAddr,
			Token: p.vaultRootToken,
			Role:  vaultRoleName,
		}
	}

	b, err = yaml.Marshal(conf)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	p.configBuf = b
	if err := os.WriteFile(filepath.Join(p.dir, "config.yaml"), b, 0644); err != nil {
		return err
	}

	return nil
}
