package heimdev

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	mrand "math/rand"
	"net/http"
	"os"
	"time"

	"github.com/caos/oidc/pkg/oidc"
	"github.com/caos/oidc/pkg/op"
	"github.com/gorilla/mux"
	"golang.org/x/xerrors"
	"gopkg.in/square/go-jose.v2"

	"go.f110.dev/heimdallr/pkg/cmd"
)

func openIDProvider(port int, signingPrivateKey string) error {
	var privateKey crypto.PrivateKey
	if _, err := os.Stat(signingPrivateKey); os.IsNotExist(err) {
		newPrivateKey, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			return xerrors.Errorf(": %w", err)
		}
		f, err := os.Create(signingPrivateKey)
		if err != nil {
			return xerrors.Errorf(": %w", err)
		}
		buf := x509.MarshalPKCS1PrivateKey(newPrivateKey)
		if err := pem.Encode(f, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: buf}); err != nil {
			return xerrors.Errorf(": %w", err)
		}
		privateKey = newPrivateKey
	} else {
		buf, err := os.ReadFile(signingPrivateKey)
		if err != nil {
			return xerrors.Errorf(": %w", err)
		}
		b, _ := pem.Decode(buf)
		loadedPrivateKey, err := x509.ParsePKCS1PrivateKey(b.Bytes)
		if err != nil {
			return xerrors.Errorf(": %w", err)
		}
		privateKey = loadedPrivateKey
	}

	if err := os.Setenv(op.OidcDevMode, "1"); err != nil {
		panic(err)
	}
	conf := &op.Config{
		Issuer:    fmt.Sprintf("http://127.0.0.1:%d/", port),
		CryptoKey: sha256.Sum256([]byte("test")),
	}

	st := newProviderStorage(privateKey)
	st.Clients = []op.Client{
		&client{
			ID:          "heim-test",
			RedirectURL: []string{"https://local-proxy.f110.dev:4000/auth/callback"},
			Login:       "/login",
		},
	}

	p, err := op.NewOpenIDProvider(context.Background(), conf, st)
	if err != nil {
		return xerrors.Errorf(": %w", err)
	}
	router := p.HttpHandler().(*mux.Router)
	router.Methods(http.MethodGet).Path("/login").HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		io.WriteString(w, `<html><body><form action="/login" method="POST">`)
		fmt.Fprintf(w, "<input type=\"hidden\" name=\"id\" value=%q>", req.URL.Query().Get("id"))
		io.WriteString(w, `
<input type="text" name="email" placeholder="email" size=30>
<input type="submit">
</form>
</body></html>`)
	})
	router.Methods(http.MethodPost).Path("/login").HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if err := req.ParseForm(); err != nil {
			log.Println(err)
			return
		}
		id := req.FormValue("id")
		email := req.FormValue("email")
		if v := st.authRequests[id]; v != nil {
			v.AuthTime = time.Now()
			v.Email = email
			http.Redirect(w, req, "/authorize/callback?id="+id, http.StatusFound)
		}
	})

	s := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: router,
	}

	return s.ListenAndServe()
}

func OpenIDProvider(rootCmd *cmd.Command) {
	port := 5001
	signingPrivateKey := ""

	opCommand := &cmd.Command{
		Use:   "op",
		Short: "Start the OpenID Provider",
		Run: func(_ context.Context, _ *cmd.Command, _ []string) error {
			mrand.Seed(time.Now().UnixNano())
			return openIDProvider(port, signingPrivateKey)
		},
	}
	opCommand.Flags().Int("port", "Listen port").Var(&port).Default(5001)
	opCommand.Flags().String("private-key", "Private key file for signing").Var(&signingPrivateKey).Required()

	rootCmd.AddCommand(opCommand)
}

type providerStorage struct {
	SigningKey       crypto.PrivateKey
	SigningPublicKey crypto.PublicKey
	Clients          []op.Client

	authRequests map[string]*authRequest
}

var _ op.Storage = &providerStorage{}

func newProviderStorage(signingKey crypto.PrivateKey) *providerStorage {
	var publicKey crypto.PublicKey
	switch v := signingKey.(type) {
	case *rsa.PrivateKey:
		publicKey = v.Public()
	}
	return &providerStorage{
		SigningKey:       signingKey,
		SigningPublicKey: publicKey,
		authRequests:     make(map[string]*authRequest),
	}
}

func (a *providerStorage) CreateAuthRequest(_ context.Context, req *oidc.AuthRequest, userId string) (op.AuthRequest, error) {
	id := randomString(10)
	a.authRequests[id] = &authRequest{
		ID:           id,
		ClientID:     req.ClientID,
		ResponseType: req.ResponseType,
		State:        req.State,
		Nonce:        req.Nonce,
		RedirectURL:  req.RedirectURI,
		Scopes:       req.Scopes,
	}
	return a.authRequests[id], nil
}

func (a *providerStorage) AuthRequestByID(_ context.Context, id string) (op.AuthRequest, error) {
	if v := a.authRequests[id]; v == nil {
		return nil, xerrors.Errorf("not found")
	} else {
		return v, nil
	}
}

func (a *providerStorage) AuthRequestByCode(ctx context.Context, code string) (op.AuthRequest, error) {
	for _, v := range a.authRequests {
		if v.Code == code {
			return v, nil
		}
	}

	return nil, xerrors.Errorf("code is not found")
}

func (a *providerStorage) SaveAuthCode(ctx context.Context, id, code string) error {
	if v := a.authRequests[id]; v == nil {
		return xerrors.Errorf("auth request id is not found")
	} else {
		v.Code = code
	}

	return nil
}

func (a *providerStorage) DeleteAuthRequest(ctx context.Context, id string) error {
	delete(a.authRequests, id)
	return nil
}

func (a *providerStorage) CreateAccessToken(ctx context.Context, req op.TokenRequest) (string, time.Time, error) {
	return "test-access-token", time.Now().Add(24 * time.Hour), nil
}

func (a *providerStorage) CreateAccessAndRefreshTokens(ctx context.Context, request op.TokenRequest, currentRefreshToken string) (accessTokenID string, newRefreshToken string, expiration time.Time, err error) {
	//TODO implement me
	panic("implement me")
}

func (a *providerStorage) TokenRequestByRefreshToken(ctx context.Context, refreshToken string) (op.RefreshTokenRequest, error) {
	//TODO implement me
	panic("implement me")
}

func (a *providerStorage) TerminateSession(ctx context.Context, userID string, clientID string) error {
	//TODO implement me
	panic("implement me")
}

func (a *providerStorage) RevokeToken(ctx context.Context, token string, userID string, clientID string) *oidc.Error {
	//TODO implement me
	panic("implement me")
}

func (a *providerStorage) GetSigningKey(ctx context.Context, keys chan<- jose.SigningKey) {
	var algo jose.SignatureAlgorithm
	switch v := a.SigningKey.(type) {
	case *ecdsa.PrivateKey:
		switch v.Params().BitSize {
		case 256:
			algo = jose.ES256
		case 384:
			algo = jose.ES384
		case 512:
			algo = jose.ES512
		}
	case *rsa.PrivateKey:
		algo = jose.RS256
	}
	keys <- jose.SigningKey{
		Algorithm: algo,
		Key:       a.SigningKey,
	}
}

func (a *providerStorage) GetKeySet(_ context.Context) (*jose.JSONWebKeySet, error) {
	return &jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{
				Key:       a.SigningPublicKey,
				KeyID:     "foobar",
				Algorithm: "RS256",
				Use:       "sig",
			},
		},
	}, nil
}

func (a *providerStorage) GetClientByClientID(ctx context.Context, clientID string) (op.Client, error) {
	for _, v := range a.Clients {
		if v.GetID() == clientID {
			return v, nil
		}
	}

	return nil, xerrors.Errorf("client %s is not found", clientID)
}

func (a *providerStorage) AuthorizeClientIDSecret(ctx context.Context, clientID, clientSecret string) error {
	for _, v := range a.Clients {
		if v.GetID() == clientID {
			// TODO: should check the client secret
			return nil
		}
	}

	return xerrors.Errorf("%s is not found", clientID)
}

func (a *providerStorage) SetUserinfoFromScopes(ctx context.Context, userinfo oidc.UserInfoSetter, userID, clientID string, scopes []string) error {
	for _, v := range scopes {
		switch v {
		case "email":
			userinfo.SetEmail(userID, true)
			userinfo.SetSubject(userID)
		}
	}

	return nil
}

func (a *providerStorage) SetUserinfoFromToken(ctx context.Context, userinfo oidc.UserInfoSetter, tokenID, subject, origin string) error {
	//TODO implement me
	panic("implement me")
}

func (a *providerStorage) SetIntrospectionFromToken(ctx context.Context, userinfo oidc.IntrospectionResponse, tokenID, subject, clientID string) error {
	//TODO implement me
	panic("implement me")
}

func (a *providerStorage) GetPrivateClaimsFromScopes(ctx context.Context, userID, clientID string, scopes []string) (map[string]interface{}, error) {
	// TODO
	return nil, nil
}

func (a *providerStorage) GetKeyByIDAndUserID(ctx context.Context, keyID, userID string) (*jose.JSONWebKey, error) {
	//TODO implement me
	panic("implement me")
}

func (a *providerStorage) ValidateJWTProfileScopes(ctx context.Context, userID string, scopes []string) ([]string, error) {
	//TODO implement me
	panic("implement me")
}

func (a *providerStorage) Health(ctx context.Context) error {
	//TODO implement me
	panic("implement me")
}

type client struct {
	ID          string
	RedirectURL []string
	Login       string
}

var _ op.Client = &client{}

func (c *client) GetID() string {
	return c.ID
}

func (c *client) RedirectURIs() []string {
	return c.RedirectURL
}

func (c *client) PostLogoutRedirectURIs() []string {
	//TODO implement me
	panic("implement me")
}

func (c *client) ApplicationType() op.ApplicationType {
	//TODO implement me
	panic("implement me")
}

func (c *client) AuthMethod() oidc.AuthMethod {
	return oidc.AuthMethodBasic
}

func (c *client) ResponseTypes() []oidc.ResponseType {
	return []oidc.ResponseType{oidc.ResponseTypeCode}
}

func (c *client) GrantTypes() []oidc.GrantType {
	return []oidc.GrantType{oidc.GrantTypeCode}
}

func (c *client) LoginURL(s string) string {
	return c.Login + "?id=" + s
}

func (c *client) AccessTokenType() op.AccessTokenType {
	return op.AccessTokenTypeJWT
}

func (c *client) IDTokenLifetime() time.Duration {
	return 24 * time.Hour
}

func (c *client) DevMode() bool {
	//TODO implement me
	panic("implement me")
}

func (c *client) RestrictAdditionalIdTokenScopes() func(scopes []string) []string {
	// TODO
	return func(scopes []string) []string {
		return scopes
	}
}

func (c *client) RestrictAdditionalAccessTokenScopes() func(scopes []string) []string {
	// TODO
	return func(scopes []string) []string {
		return []string{}
	}
}

func (c *client) IsScopeAllowed(scope string) bool {
	//TODO implement me
	panic("implement me")
}

func (c *client) IDTokenUserinfoClaimsAssertion() bool {
	return true
}

func (c *client) ClockSkew() time.Duration {
	return time.Minute
}

type authRequest struct {
	ID           string
	ClientID     string
	ResponseType oidc.ResponseType
	Code         string
	State        string
	Nonce        string
	RedirectURL  string
	Scopes       []string

	AuthTime time.Time
	Email    string
}

var _ op.AuthRequest = &authRequest{}

func (a *authRequest) GetID() string {
	return a.ID
}

func (a *authRequest) GetACR() string {
	// TODO
	return ""
}

func (a *authRequest) GetAMR() []string {
	// TODO
	return nil
}

func (a *authRequest) GetAudience() []string {
	// TODO
	return []string{}
}

func (a *authRequest) GetAuthTime() time.Time {
	return a.AuthTime
}

func (a *authRequest) GetClientID() string {
	return a.ClientID
}

func (a *authRequest) GetCodeChallenge() *oidc.CodeChallenge {
	//TODO implement me
	panic("implement me")
}

func (a *authRequest) GetNonce() string {
	return a.Nonce
}

func (a *authRequest) GetRedirectURI() string {
	return a.RedirectURL
}

func (a *authRequest) GetResponseType() oidc.ResponseType {
	return a.ResponseType
}

func (a *authRequest) GetResponseMode() oidc.ResponseMode {
	return oidc.ResponseModeQuery
}

func (a *authRequest) GetScopes() []string {
	return a.Scopes
}

func (a *authRequest) GetState() string {
	return a.State
}

func (a *authRequest) GetSubject() string {
	return a.Email
}

func (a *authRequest) Done() bool {
	return true
}

var charset = []byte("abcdefghijklmnopqrstuvwxyz0123456789")

func randomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[mrand.Intn(len(charset))]
	}

	return string(b)
}
