package acme

import (
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/pkg/errors"
	"github.com/smallstep/certificates/authority/provisioner"
	database "github.com/smallstep/certificates/db"
	"github.com/smallstep/cli/jose"
	"github.com/smallstep/nosql"
)

// Interface is the acme authority interface.
type Interface interface {
	GetLink(Link, string, bool, ...string) string
	GetLinkFromBaseURL(Link, string, bool, string, ...string) string

	DeactivateAccount(ctx context.Context, accID string) (*Account, error)
	GetAccount(ctx context.Context, accID string) (*Account, error)
	GetAccountByKey(ctx context.Context, key *jose.JSONWebKey) (*Account, error)
	NewAccount(ctx context.Context, ao AccountOptions) (*Account, error)
	UpdateAccount(context.Context, string, []string) (*Account, error)

	GetAuthz(ctx context.Context, accID string, authzID string) (*Authz, error)
	GetDirectory(ctx context.Context) (*Directory, error)
	GetCertificate(string, string) ([]byte, error)
	ValidateChallenge(ctx context.Context, accID string, chID string, key *jose.JSONWebKey) (*Challenge, error)

	FinalizeOrder(ctx context.Context, accID string, orderID string, csr *x509.CertificateRequest) (*Order, error)
	GetOrder(ctx context.Context, accID string, orderID string) (*Order, error)
	GetOrdersByAccount(ctx context.Context, accID string) ([]string, error)
	NewOrder(ctx context.Context, oo OrderOptions) (*Order, error)

	LoadProvisionerByID(string) (provisioner.Interface, error)
	NewNonce() (string, error)
	UseNonce(string) error
}

// Authority is the layer that handles all ACME interactions.
type Authority struct {
	db       nosql.DB
	dir      *directory
	signAuth SignAuthority
}

var (
	accountTable           = []byte("acme_accounts")
	accountByKeyIDTable    = []byte("acme_keyID_accountID_index")
	authzTable             = []byte("acme_authzs")
	challengeTable         = []byte("acme_challenges")
	nonceTable             = []byte("nonces")
	orderTable             = []byte("acme_orders")
	ordersByAccountIDTable = []byte("acme_account_orders_index")
	certTable              = []byte("acme_certs")
)

func baseURLFromContext(ctx context.Context) string {
	val, ok := ctx.Value(BaseURLContextKey).(string)
	if !ok || val == "" {
		return ""
	}
	return val
}
func provisionerFromContext(ctx context.Context) (provisioner.Interface, error) {
	val, ok := ctx.Value(ProvisionerContextKey).(provisioner.Interface)
	if !ok || val == nil {
		return nil, ServerInternalErr(errors.Errorf("provisioner expected in request context"))
	}
	return val, nil
}

// NewAuthority returns a new Authority that implements the ACME interface.
func NewAuthority(db nosql.DB, dns, prefix string, signAuth SignAuthority) (*Authority, error) {
	if _, ok := db.(*database.SimpleDB); !ok {
		// If it's not a SimpleDB then go ahead and bootstrap the DB with the
		// necessary ACME tables. SimpleDB should ONLY be used for testing.
		tables := [][]byte{accountTable, accountByKeyIDTable, authzTable,
			challengeTable, nonceTable, orderTable, ordersByAccountIDTable,
			certTable}
		for _, b := range tables {
			if err := db.CreateTable(b); err != nil {
				return nil, errors.Wrapf(err, "error creating table %s",
					string(b))
			}
		}
	}
	return &Authority{
		db: db, dir: newDirectory(dns, prefix), signAuth: signAuth,
	}, nil
}

// GetLink returns the requested link from the directory.
func (a *Authority) GetLink(typ Link, provID string, abs bool, inputs ...string) string {
	return a.dir.getLink(typ, provID, abs, inputs...)
}

// GetLinkFromBaseURL returns the requested link from the directory using the
// baseURL from the request.
func (a *Authority) GetLinkFromBaseURL(typ Link, provID string, abs bool, baseURLFromRequest string, inputs ...string) string {
	return a.dir.getLinkFromBaseURL(typ, provID, abs, baseURLFromRequest, inputs...)
}

// GetDirectory returns the ACME directory object.
func (a *Authority) GetDirectory(ctx context.Context) (*Directory, error) {
	p, err := provisionerFromContext(ctx)
	if err != nil {
		fmt.Println("I AM HERE")
		return nil, err
	}
	name := url.PathEscape(p.GetName())
	bu := baseURLFromContext(ctx)
	return &Directory{
		NewNonce:   a.dir.getLinkFromBaseURL(NewNonceLink, name, true, bu),
		NewAccount: a.dir.getLinkFromBaseURL(NewAccountLink, name, true, bu),
		NewOrder:   a.dir.getLinkFromBaseURL(NewOrderLink, name, true, bu),
		RevokeCert: a.dir.getLinkFromBaseURL(RevokeCertLink, name, true, bu),
		KeyChange:  a.dir.getLinkFromBaseURL(KeyChangeLink, name, true, bu),
	}, nil
}

// LoadProvisionerByID calls out to the SignAuthority interface to load a
// provisioner by ID.
func (a *Authority) LoadProvisionerByID(id string) (provisioner.Interface, error) {
	return a.signAuth.LoadProvisionerByID(id)
}

// NewNonce generates, stores, and returns a new ACME nonce.
func (a *Authority) NewNonce() (string, error) {
	n, err := newNonce(a.db)
	if err != nil {
		return "", err
	}
	return n.ID, nil
}

// UseNonce consumes the given nonce if it is valid, returns error otherwise.
func (a *Authority) UseNonce(nonce string) error {
	return useNonce(a.db, nonce)
}

// NewAccount creates, stores, and returns a new ACME account.
func (a *Authority) NewAccount(ctx context.Context, ao AccountOptions) (*Account, error) {
	acc, err := newAccount(a.db, ao)
	if err != nil {
		return nil, err
	}
	return acc.toACME(ctx, a.db, a.dir)
}

// UpdateAccount updates an ACME account.
func (a *Authority) UpdateAccount(ctx context.Context, id string, contact []string) (*Account, error) {
	acc, err := getAccountByID(a.db, id)
	if err != nil {
		return nil, ServerInternalErr(err)
	}
	if acc, err = acc.update(a.db, contact); err != nil {
		return nil, err
	}
	return acc.toACME(ctx, a.db, a.dir)
}

// GetAccount returns an ACME account.
func (a *Authority) GetAccount(ctx context.Context, id string) (*Account, error) {
	acc, err := getAccountByID(a.db, id)
	if err != nil {
		return nil, err
	}
	return acc.toACME(ctx, a.db, a.dir)
}

// DeactivateAccount deactivates an ACME account.
func (a *Authority) DeactivateAccount(ctx context.Context, id string) (*Account, error) {
	acc, err := getAccountByID(a.db, id)
	if err != nil {
		return nil, err
	}
	if acc, err = acc.deactivate(a.db); err != nil {
		return nil, err
	}
	return acc.toACME(ctx, a.db, a.dir)
}

func keyToID(jwk *jose.JSONWebKey) (string, error) {
	kid, err := jwk.Thumbprint(crypto.SHA256)
	if err != nil {
		return "", ServerInternalErr(errors.Wrap(err, "error generating jwk thumbprint"))
	}
	return base64.RawURLEncoding.EncodeToString(kid), nil
}

// GetAccountByKey returns the ACME associated with the jwk id.
func (a *Authority) GetAccountByKey(ctx context.Context, jwk *jose.JSONWebKey) (*Account, error) {
	kid, err := keyToID(jwk)
	if err != nil {
		return nil, err
	}
	acc, err := getAccountByKeyID(a.db, kid)
	if err != nil {
		return nil, err
	}
	return acc.toACME(ctx, a.db, a.dir)
}

// GetOrder returns an ACME order.
func (a *Authority) GetOrder(ctx context.Context, accID, orderID string) (*Order, error) {
	o, err := getOrder(a.db, orderID)
	if err != nil {
		return nil, err
	}
	if accID != o.AccountID {
		return nil, UnauthorizedErr(errors.New("account does not own order"))
	}
	if o, err = o.updateStatus(a.db); err != nil {
		return nil, err
	}
	return o.toACME(ctx, a.db, a.dir)
}

// GetOrdersByAccount returns the list of order urls owned by the account.
func (a *Authority) GetOrdersByAccount(ctx context.Context, id string) ([]string, error) {
	prov, err := provisionerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	provName := URLSafeProvisionerName(prov)
	baseURL := baseURLFromContext(ctx)

	oids, err := getOrderIDsByAccount(a.db, id)
	if err != nil {
		return nil, err
	}

	var ret = []string{}
	for _, oid := range oids {
		o, err := getOrder(a.db, oid)
		if err != nil {
			return nil, ServerInternalErr(err)
		}
		if o.Status == StatusInvalid {
			continue
		}
		ret = append(ret, a.dir.getLinkFromBaseURL(OrderLink, provName, true, baseURL, o.ID))
	}
	return ret, nil
}

// NewOrder generates, stores, and returns a new ACME order.
func (a *Authority) NewOrder(ctx context.Context, ops OrderOptions) (*Order, error) {
	order, err := newOrder(a.db, ops)
	if err != nil {
		return nil, Wrap(err, "error creating order")
	}
	return order.toACME(ctx, a.db, a.dir)
}

// FinalizeOrder attempts to finalize an order and generate a new certificate.
func (a *Authority) FinalizeOrder(ctx context.Context, accID, orderID string, csr *x509.CertificateRequest) (*Order, error) {
	prov, err := provisionerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	o, err := getOrder(a.db, orderID)
	if err != nil {
		return nil, err
	}
	if accID != o.AccountID {
		return nil, UnauthorizedErr(errors.New("account does not own order"))
	}
	o, err = o.finalize(a.db, csr, a.signAuth, prov)
	if err != nil {
		return nil, Wrap(err, "error finalizing order")
	}
	return o.toACME(ctx, a.db, a.dir)
}

// GetAuthz retrieves and attempts to update the status on an ACME authz
// before returning.
func (a *Authority) GetAuthz(ctx context.Context, accID, authzID string) (*Authz, error) {
	az, err := getAuthz(a.db, authzID)
	if err != nil {
		return nil, err
	}
	if accID != az.getAccountID() {
		return nil, UnauthorizedErr(errors.New("account does not own authz"))
	}
	az, err = az.updateStatus(a.db)
	if err != nil {
		return nil, Wrap(err, "error updating authz status")
	}
	return az.toACME(ctx, a.db, a.dir)
}

// ValidateChallenge attempts to validate the challenge.
func (a *Authority) ValidateChallenge(ctx context.Context, accID, chID string, jwk *jose.JSONWebKey) (*Challenge, error) {
	ch, err := getChallenge(a.db, chID)
	if err != nil {
		return nil, err
	}
	if accID != ch.getAccountID() {
		return nil, UnauthorizedErr(errors.New("account does not own challenge"))
	}
	client := http.Client{
		Timeout: time.Duration(30 * time.Second),
	}
	dialer := &net.Dialer{
		Timeout: 30 * time.Second,
	}
	ch, err = ch.validate(a.db, jwk, validateOptions{
		httpGet:   client.Get,
		lookupTxt: net.LookupTXT,
		tlsDial: func(network, addr string, config *tls.Config) (*tls.Conn, error) {
			return tls.DialWithDialer(dialer, network, addr, config)
		},
	})
	if err != nil {
		return nil, Wrap(err, "error attempting challenge validation")
	}
	return ch.toACME(ctx, a.db, a.dir)
}

// GetCertificate retrieves the Certificate by ID.
func (a *Authority) GetCertificate(accID, certID string) ([]byte, error) {
	cert, err := getCert(a.db, certID)
	if err != nil {
		return nil, err
	}
	if accID != cert.AccountID {
		return nil, UnauthorizedErr(errors.New("account does not own certificate"))
	}
	return cert.toACME(a.db, a.dir)
}
